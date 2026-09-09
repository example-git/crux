package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorOAuthLiteralCredentialHTTPS(t *testing.T) {
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	t.Setenv("CRUX_LITERAL_ORDINARY", "resolved-key")
	received := make(chan string, 3)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fixture","object":"chat.completion","model":"main","choices":[{"message":{"role":"assistant","content":"literal accepted"},"finish_reason":"stop"}]}`)
	}))
	defer host.Close()
	previous := http.DefaultClient
	http.DefaultClient = host.Client()
	defer func() { http.DefaultClient = previous }()
	marker := filepath.Join(t.TempDir(), "must-not-run")
	literal := "synthetic-$(touch " + marker + ")-$CRUX_LITERAL_ORDINARY"
	for _, mode := range []string{"literal", "expression", "nonmatching-oauth"} {
		t.Run(mode, func(t *testing.T) {
			key, want := literal, literal
			token := &oauth.Token{AccessToken: literal}
			if mode != "literal" {
				key = "$CRUX_LITERAL_ORDINARY"
				want = "resolved-key"
				if mode == "expression" {
					token = nil
				}
			}
			cfg := authenticationRevocationRuntimeConfig(host.URL+"/v1", key)
			p, _ := cfg.Providers.Get("fixture")
			p.OAuthToken = token
			cfg.Providers.Set("fixture", p)
			store := config.NewTestStore(cfg)
			coord := &coordinator{cfg: store}
			large, small, err := coord.buildAgentModels(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false)
			require.NoError(t, err)
			require.Equal(t, cfg.Models[config.SelectedModelTypeSmall], small.ModelCfg)
			result, err := large.Model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("credential fixture")}})
			require.NoError(t, err)
			require.NotEmpty(t, result.Content)
			require.Equal(t, "Bearer "+want, <-received)
			require.NoFileExists(t, marker)
		})
	}
}
