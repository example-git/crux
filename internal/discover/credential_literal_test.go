package discover

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

type literalCredentialResolver struct {
	token    string
	keyCalls int
}

func (r *literalCredentialResolver) ResolveValue(value string) (string, error) {
	if value == r.token {
		r.keyCalls++
		return "expanded-key", nil
	}
	if value == "$HEADER" {
		return "resolved-header", nil
	}
	if value == "$FAIL" {
		return "", errors.New("fixture failed")
	}
	return value, nil
}

func TestDiscoverOAuthLiteralCredentialHTTPS(t *testing.T) {
	const token = "synthetic-$VAR-$(command)"
	requests := make(chan string, 2)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Authorization")
		require.Equal(t, "resolved-header", r.Header.Get("X-Fixture"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"discovered"}]}`))
	}))
	defer host.Close()
	previous := httpClient
	httpClient = host.Client()
	defer func() { httpClient = previous }()
	resolver := &literalCredentialResolver{token: token}
	cfg := Config{ID: "fixture", BaseURL: host.URL, APIKey: token, APIKeyLiteral: true, ExtraHeaders: map[string]string{"X-Fixture": "$HEADER"}}
	models, err := DiscoverModels(t.Context(), cfg, resolver)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "discovered", models[0].ID)
	require.Equal(t, "Bearer "+token, <-requests)
	require.Zero(t, resolver.keyCalls)
	cfg.APIKeyLiteral = false
	_, err = DiscoverModels(t.Context(), cfg, resolver)
	require.NoError(t, err)
	require.Equal(t, "Bearer expanded-key", <-requests)
	require.Equal(t, 1, resolver.keyCalls)
}
