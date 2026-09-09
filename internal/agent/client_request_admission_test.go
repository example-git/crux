package agent

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestClientRetainedRequestAdmissionUsesCurrentAuthority(t *testing.T) {
	var mu sync.Mutex
	var credentials []string
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		credentials = append(credentials, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture","object":"chat.completion","model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"accepted"},"finish_reason":"stop"}]}`))
	}))
	defer host.Close()
	originalClient := http.DefaultClient
	http.DefaultClient = host.Client()
	defer func() { http.DefaultClient = originalClient }()
	replacement := clientResponsesProposal(t, host.URL+"/v1")
	id := replacement.Providers[0].Config.ID
	principal := strings.Repeat("a", 64)
	for _, mode := range []string{"newer available", "disabled", "exact owner replacement", "principal", "server mode", "older revision", "same revision changed"} {
		t.Run(mode, func(t *testing.T) {
			selected := config.SelectedModel{Provider: id, Model: "model"}
			proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1, Providers: []config.RemoteProviderDefinition{{Config: config.ProviderConfig{ID: id, Type: catalog.TypeOpenAICompat, BaseURL: host.URL + "/v1", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Models: []catalog.Model{{ID: "model", Name: "Model", ContextWindow: 8192, DefaultMaxTokens: 1024}}}}}, Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: selected, config.SelectedModelTypeSmall: selected}, Credentials: []config.RemoteCredentialBinding{{Owner: providerregistry.RegistrationOwner{ProviderID: id}, Generation: 1, APIKey: "synthetic-captured"}}}
			compile := func(value config.RemoteRuntimeProposal, principal string) *config.ConfigStore {
				sealClientResponsesProposal(t, &value)
				root := t.TempDir()
				store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "data"), false, value, principal, env.NewFromMap(map[string]string{}))
				require.NoError(t, err)
				return store
			}
			if mode == "older revision" {
				proposal.Revision = 2
				proposal.Credentials[0].Generation = 2
			}
			store := compile(proposal, principal)
			coord := &coordinator{cfg: store}
			snapshot := store.RuntimeSnapshot()
			provider, _ := snapshot.Config().Providers.Get(id)
			built, err := coord.buildProvider(snapshot, provider, selected, false)
			require.NoError(t, err)
			retained, err := built.LanguageModel(t.Context(), selected.Model)
			require.NoError(t, err)
			call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("initial request")}}
			_, err = retained.Generate(t.Context(), call)
			require.NoError(t, err)
			mu.Lock()
			before := len(credentials)
			mu.Unlock()
			expected := "current client runtime authority changed"
			switch mode {
			case "newer available", "disabled":
				proposal.Revision = 2
				proposal.Credentials[0].Generation = 2
				proposal.Credentials[0].APIKey = "synthetic-current"
				proposal.Controls.ResponseVerbosity = "high"
				proposal.Providers[0].Config.Disable = mode == "disabled"
				sealClientResponsesProposal(t, &proposal)
				_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
				require.NoError(t, err)
				expected = "disabled"
			case "exact owner replacement":
				next := replacement
				next.Revision = 2
				next.Credentials[0].Generation = 2
				sealClientResponsesProposal(t, &next)
				_, err = store.ReplaceRemoteRuntime(t.Context(), next, principal, 1)
				require.NoError(t, err)
				expected = "owner is unavailable or changed"
			case "principal":
				coord.cfg = compile(proposal, strings.Repeat("b", 64))
			case "server mode":
				coord.cfg = config.NewTestStore(snapshot.Config())
			case "older revision":
				proposal.Revision = 1
				proposal.Credentials[0].Generation = 1
				coord.cfg = compile(proposal, principal)
			case "same revision changed":
				proposal.Providers[0].Config.Name = "changed"
				coord.cfg = compile(proposal, principal)
			}
			_, err = retained.Generate(t.Context(), call)
			if mode == "newer available" {
				require.NoError(t, err)
				mu.Lock()
				require.Len(t, credentials, before+1)
				require.Equal(t, "Bearer synthetic-captured", credentials[len(credentials)-1])
				mu.Unlock()
			} else {
				require.ErrorContains(t, err, expected)
				stream, streamErr := retained.Stream(t.Context(), call)
				if streamErr == nil {
					for event := range stream {
						if event.Error != nil {
							streamErr = event.Error
							break
						}
					}
				}
				require.ErrorContains(t, streamErr, expected)
				mu.Lock()
				require.Len(t, credentials, before, "refused new calls must not reach HTTPS with any credential")
				mu.Unlock()
			}
		})
	}
}
