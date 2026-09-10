package imagegen

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestImageProviderOAuthLiteralCredentialHTTPS(t *testing.T) {
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	t.Setenv("CRUX_IMAGE_LITERAL", "resolved-image-key")
	requests := make(chan string, 3)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	defer host.Close()
	marker := filepath.Join(t.TempDir(), "must-not-run")
	literal := "synthetic-$(touch " + marker + ")-$CRUX_IMAGE_LITERAL"
	for _, mode := range []string{"literal", "expression", "codex"} {
		t.Run(mode, func(t *testing.T) {
			id, key, want := "openai", literal, literal
			token := &oauth.Token{AccessToken: literal, ExpiresAt: time.Now().Add(time.Hour).Unix()}
			owner := &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}
			if mode == "expression" {
				key = "$CRUX_IMAGE_LITERAL"
				want = "resolved-image-key"
				token = nil
			}
			if mode == "codex" {
				id = "codex"
				owner = &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex}
				previous := codexBaseURLOverride
				codexBaseURLOverride = host.URL
				defer func() { codexBaseURLOverride = previous }()
			}
			store := config.NewTestStore(&config.Config{Options: &config.Options{DataDirectory: t.TempDir()}, Providers: csync.NewMapFrom(map[string]config.ProviderConfig{id: {ID: id, Type: catalog.TypeOpenAICompat, APIKey: key, OAuthToken: token, BaseURL: host.URL, Owner: owner}})})
			client := NewProviderClient(store)
			client.HTTPClient = host.Client()
			response, err := client.Generate(t.Context(), GenerateRequest{Prompt: "literal credential fixture", N: 1})
			require.NoError(t, err)
			require.NotNil(t, response)
			require.Equal(t, "Bearer "+want, <-requests)
			require.NoFileExists(t, marker)
		})
	}
}
