package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestPluginInferencePreservesExactDeclaredUserAgent(t *testing.T) {
	for _, literal := range []bool{false, true} {
		expected := "plugin-target-cli/1.2.3"
		userAgent := &manifest.Template{Kind: "context", Ref: "client.user_agent"}
		if literal {
			expected = "ExactTargetClient/4.5.6 (native platform)"
			userAgent = &manifest.Template{Kind: "literal", Value: expected}
		}
		t.Run(expected, func(t *testing.T) {
			requests := 0
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				require.Equal(t, expected, r.Header.Get("User-Agent"))
				_, _ = w.Write([]byte(`{"text":"verified"}`))
			}))
			defer host.Close()
			registration, err := providerregistry.FromManifest(manifest.Manifest{
				ID: "test.target-identity", Version: "1.0.0", Provider: manifest.Provider{ID: "target-identity", Name: "Target Identity"},
				Capabilities: manifest.Capabilities{
					Endpoints:        []manifest.Endpoint{{ID: "api", BaseURL: host.URL}},
					ClientIdentities: map[string]manifest.ResolvedClientIdentity{"target": {CacheKey: "plugin-target-identity", FallbackVersion: "1.2.3", VersionPattern: `^[0-9]+\.[0-9]+\.[0-9]+$`, UserAgentFormat: "plugin-target-cli/{version}", ProbeTimeoutMS: 1000, ProbeMaxBytes: 1024}},
					Operations:       []manifest.Operation{{ID: "inference", Kind: "inference", Protocol: string(providerregistry.ConstructionGenericJSON), Transport: "http-json", Endpoint: "api", Method: http.MethodPost, Path: "/generate", ClientIdentity: "target"}},
					Headers:          []manifest.HeaderRule{{Operation: "set", Name: "User-Agent", Value: userAgent, Protected: true}},
				},
			})
			require.NoError(t, err)
			provider := config.ProviderConfig{ID: registration.ProviderID, BaseURL: host.URL, ExtraHeaders: map[string]string{"user-agent": "must-not-replace-plugin"}, Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction}, Plugin: &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}}
			store := config.NewTestStoreWithRegistrations(&config.Config{Options: &config.Options{}, Providers: csync.NewMapFrom(map[string]config.ProviderConfig{provider.ID: provider})}, registration)
			coord := &coordinator{cfg: store}
			built, err := coord.buildProvider(store.RuntimeSnapshot(), provider, config.SelectedModel{Provider: provider.ID, Model: "native-model"}, false)
			require.NoError(t, err)
			model, err := built.LanguageModel(t.Context(), "native-model")
			require.NoError(t, err)
			_, err = model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("verify plugin identity")}})
			require.NoError(t, err)
			require.Equal(t, 1, requests)
		})
	}
}
