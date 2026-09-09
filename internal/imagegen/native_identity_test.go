package imagegen

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func nativeImageRuntime(t *testing.T, raw json.RawMessage) (*config.ConfigStore, config.RemoteRuntimeProposal) {
	t.Helper()
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("codex")
	require.True(t, ok)
	proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers:   []config.RemoteProviderDefinition{{NativeIdentity: &config.NativeIdentity{UserAgent: "image-owner/1.2.3 (OwnerOS 1; fixture) OwnerTerminal", Version: "1.2.3", Originator: "image-owner"}, Config: config.ProviderConfig{ID: "codex", Name: "Fixture", Type: catalog.TypeOpenAICompat, BaseURL: "wss://fixture.invalid/responses", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex}, Models: []catalog.Model{{ID: "fixture", Name: "Fixture"}}}}},
		Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: "codex", Model: "fixture"}, config.SelectedModelTypeSmall: {Provider: "codex", Model: "fixture"}},
		Credentials: []config.RemoteCredentialBinding{{Owner: registration.Owner(), Generation: 1, Account: &accounts.Entry{ID: "selected", AccessToken: "synthetic-image-access", RefreshToken: "synthetic-image-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Raw: raw}}},
	}
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	store, err := config.CompileRemoteRuntime(t.TempDir(), t.TempDir(), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{}))
	require.NoError(t, err)
	return store, proposal
}

func TestClientImageNativeHeadersRetainAcceptedRuntime(t *testing.T) {
	for _, mode := range []string{"captured-account", "absent-account-metadata"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AI_CLI_DIR", t.TempDir())
			t.Setenv("CODEX_VERSION", "9.9.9")
			t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "hostile-image-origin")
			t.Setenv("TERM_PROGRAM", "HostileTerminal")
			require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, accounts.Entry{ID: "host-account", AccessToken: "synthetic-image-access", Raw: json.RawMessage(`{"account_id":"hostile-account-id"}`)}))
			raw, wantedAccount := json.RawMessage(`{"account_id":"owner-account-id"}`), "owner-account-id"
			if mode == "absent-account-metadata" {
				raw, wantedAccount = nil, ""
			}
			store, proposal := nativeImageRuntime(t, raw)
			captured, err := resolveConfiguredAuth(t.Context(), store)
			require.NoError(t, err)
			headers := make(chan http.Header, 4)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers <- r.Header.Clone()
				require.Equal(t, "Bearer synthetic-image-access", r.Header.Get("Authorization"))
				require.Equal(t, "/images/generations", r.URL.Path)
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, `{"data":[{"b64_json":"aW1hZ2U="}]}`)
			}))
			defer server.Close()
			original := codexBaseURLOverride
			codexBaseURLOverride = server.URL
			t.Cleanup(func() { codexBaseURLOverride = original })
			client := NewProviderClient(store)
			client.HTTPClient = server.Client()
			_, err = client.Generate(t.Context(), GenerateRequest{Prompt: "accepted image", N: 1})
			require.NoError(t, err)
			oldIdentity := *proposal.Providers[0].NativeIdentity
			assertHeader := func(want config.NativeIdentity) {
				t.Helper()
				actual := <-headers
				require.Equal(t, want.UserAgent, actual.Get("User-Agent"))
				require.Equal(t, want.Version, actual.Get("version"))
				require.Equal(t, want.Originator, actual.Get("originator"))
				require.Equal(t, wantedAccount, actual.Get("ChatGPT-Account-ID"))
			}
			assertHeader(oldIdentity)
			nextIdentity := config.NativeIdentity{UserAgent: "next-image/2.3.4 (NextOS 2; fixture) NextTerminal", Version: "2.3.4", Originator: "next-image"}
			proposal.Providers[0].NativeIdentity = &nextIdentity
			proposal.Revision, proposal.Credentials[0].Generation = 2, 2
			proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
			require.NoError(t, err)
			_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, strings.Repeat("a", 64), 1)
			require.NoError(t, err)
			_, err = client.Generate(t.Context(), GenerateRequest{Prompt: "next image", N: 1})
			require.NoError(t, err)
			assertHeader(nextIdentity)
			// A request prepared by the production resolver before replacement retains
			// its credential and identity when Generate later consumes that capture.
			client.authResolver = func(context.Context) (resolvedAuth, error) { return captured, nil }
			_, err = client.Generate(t.Context(), GenerateRequest{Prompt: "retained image", N: 1})
			require.NoError(t, err)
			assertHeader(oldIdentity)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = client.Generate(ctx, GenerateRequest{Prompt: "canceled image", N: 1})
			require.ErrorContains(t, err, "context canceled")
			require.Empty(t, headers)
		})
	}
}
