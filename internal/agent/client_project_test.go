package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type clientProjectRoundTrip func(*http.Request) (*http.Response, error)

func (f clientProjectRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientGeminiProjectIgnoresExecutionHostOverride(t *testing.T) {
	for _, mode := range []string{"explicit", "credential-lookup", "missing-metadata", "server-owned"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GEMINI_PROJECT_ID", "execution-host-project")
			t.Setenv("ANTIGRAVITY_CLI_VERSION", "test")
			project := ""
			expected := "credential-project"
			if mode == "explicit" {
				project, expected = "client-project", "client-project"
			}
			if mode == "missing-metadata" {
				expected = ""
			}
			if mode == "server-owned" {
				expected = "execution-host-project"
			}
			var expectedProject atomic.Value
			expectedProject.Store(expected)
			var lookups, inferences atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer synthetic-client-token", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, ":loadCodeAssist") {
					lookups.Add(1)
					if mode == "missing-metadata" {
						_, _ = w.Write([]byte(`{}`))
						return
					}
					_, _ = w.Write([]byte(`{"cloudaicompanionProject":"credential-project"}`))
					return
				}
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				assert.Equal(t, expectedProject.Load().(string), gjson.GetBytes(body, "project").String())
				inferences.Add(1)
				_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"project authority verified"}]},"finishReason":"STOP"}]}}`))
			}))
			defer host.Close()
			local, err := url.Parse(host.URL)
			require.NoError(t, err)
			transport := host.Client().Transport
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: clientProjectRoundTrip(func(r *http.Request) (*http.Response, error) {
				// Only the fixed metadata endpoint is redirected in this fixture. Every
				// request still executes over the disposable HTTPS connection.
				if strings.HasSuffix(r.URL.Path, ":loadCodeAssist") {
					copy := r.Clone(r.Context())
					target := *r.URL
					target.Scheme, target.Host = local.Scheme, local.Host
					copy.URL, copy.Host = &target, local.Host
					return transport.RoundTrip(copy)
				}
				require.Equal(t, local.Host, r.URL.Host, "unexpected external request")
				return transport.RoundTrip(r)
			})}
			defer func() { http.DefaultClient = previous }()
			registry, err := providerregistry.New(providerregistry.Integrated()...)
			require.NoError(t, err)
			registration, ok := registry.Lookup(gemini.ID)
			require.True(t, ok)
			selected := config.SelectedModel{Provider: gemini.ID, Model: "fixture"}
			proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1,
				Providers:   []config.RemoteProviderDefinition{{GeminiProjectID: &project, Config: config.ProviderConfig{ID: gemini.ID, Type: catalog.TypeOpenAICompat, BaseURL: host.URL, Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionGeminiAntigravity}, Models: []catalog.Model{{ID: selected.Model, Name: "Fixture"}}}}},
				Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: selected, config.SelectedModelTypeSmall: selected},
				Credentials: []config.RemoteCredentialBinding{{Owner: registration.Owner(), Generation: 1, Account: &accounts.Entry{ID: "selected", AccessToken: "synthetic-client-token", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}}},
			}
			sealClientResponsesProposal(t, &proposal)
			root := t.TempDir()
			store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{}))
			require.NoError(t, err)
			coord := &coordinator{cfg: store}
			providerConfig, _ := store.Config().Providers.Get(gemini.ID)
			var provider fantasy.Provider
			if mode == "server-owned" {
				provider, err = coord.buildGeminiAntigravityProvider(registration, host.URL, "synthetic-client-token", nil, func() error { return nil })
			} else {
				provider, err = coord.buildProvider(store.RuntimeSnapshot(), providerConfig, selected, false)
			}
			require.NoError(t, err)
			model, err := provider.LanguageModel(t.Context(), selected.Model)
			require.NoError(t, err)
			result, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("verify client project")}})
			require.NoError(t, err)
			require.Contains(t, result.Content.Text(), "project authority verified")
			expectedInferences := 1
			if mode == "explicit" {
				proposal.Revision, proposal.Credentials[0].Generation = 2, 2
				proposal.Providers[0].GeminiProjectID = new("new-client-project")
				sealClientResponsesProposal(t, &proposal)
				_, err := store.ReplaceRemoteRuntime(t.Context(), proposal, strings.Repeat("a", 64), 1)
				require.NoError(t, err)
				nextConfig, _ := store.Config().Providers.Get(gemini.ID)
				nextProvider, err := coord.buildProvider(store.RuntimeSnapshot(), nextConfig, selected, false)
				require.NoError(t, err)
				nextModel, err := nextProvider.LanguageModel(t.Context(), selected.Model)
				require.NoError(t, err)
				for index, captured := range []fantasy.LanguageModel{nextModel, model} {
					if index == 0 {
						expectedProject.Store("new-client-project")
					} else {
						expectedProject.Store("client-project")
					}
					result, err := captured.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("verify captured project")}})
					require.NoError(t, err)
					require.Contains(t, result.Content.Text(), "project authority verified")
				}
				expectedInferences = 3
			}
			require.EqualValues(t, expectedInferences, inferences.Load())
			if mode == "credential-lookup" || mode == "missing-metadata" {
				require.EqualValues(t, 1, lookups.Load())
			} else {
				require.Zero(t, lookups.Load())
			}
		})
	}
}
