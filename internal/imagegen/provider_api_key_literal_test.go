package imagegen

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestImageProviderClientResolvedAPIKeyHTTPS(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "must-not-execute")
	literal := "synthetic-$(printf x > " + marker + ")-$UNSET"
	requests := make(chan string, 1)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	t.Cleanup(host.Close)
	provider := config.ProviderConfig{ID: "openai", Type: catalog.TypeOpenAICompat, BaseURL: host.URL,
		Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Models: []catalog.Model{{ID: "model"}}}
	proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers:   []config.RemoteProviderDefinition{{Config: provider}},
		Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: provider.ID, Model: "model"}, config.SelectedModelTypeSmall: {Provider: provider.ID, Model: "model"}},
		Credentials: []config.RemoteCredentialBinding{{Owner: providerregistry.RegistrationOwner{ProviderID: provider.ID}, Generation: 1, APIKey: literal}}}
	compile := func() *config.ConfigStore {
		var err error
		proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
		require.NoError(t, err)
		store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "workspace"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root, "UNSET": "must-not-substitute"}))
		require.NoError(t, err)
		return store
	}
	store := compile()
	client := NewProviderClient(store)
	client.HTTPClient = host.Client()
	response, err := client.Generate(t.Context(), GenerateRequest{Prompt: "literal image credential", N: 1})
	require.NoError(t, err)
	require.Equal(t, AuthAPIKey, response.AuthMode)
	select {
	case header := <-requests:
		require.Equal(t, "Bearer "+literal, header)
	case <-time.After(time.Second):
		t.Fatal("expected actual image HTTPS request")
	}
	require.NoFileExists(t, marker)
	// Resolution preserves bytes before HTTP's own header serialization rules.
	proposal.Credentials[0].APIKey = " " + literal + " "
	store = compile()
	auth, configured, err := configuredOpenAIAuth(store, store.RuntimeSnapshot())
	require.NoError(t, err)
	require.True(t, configured)
	require.Equal(t, proposal.Credentials[0].APIKey, auth.token)
}
