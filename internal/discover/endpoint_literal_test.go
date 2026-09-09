package discover

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

type literalEndpointResolver struct {
	endpoint, other string
	calls           int
}

func (r *literalEndpointResolver) ResolveValue(value string) (string, error) {
	if value == r.endpoint {
		r.calls++
		return r.other, nil
	}
	return value, nil
}
func TestDiscoverCheckedEndpointUsesOriginalDestination(t *testing.T) {
	var first, second int
	original := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first++
		_, _ = w.Write([]byte(`{"data":[{"id":"original"}]}`))
	}))
	defer original.Close()
	changed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second++
		_, _ = w.Write([]byte(`{"data":[{"id":"changed"}]}`))
	}))
	defer changed.Close()
	prior := httpClient
	httpClient = original.Client()
	defer func() { httpClient = prior }()
	resolver := &literalEndpointResolver{endpoint: original.URL, other: changed.URL}
	cfg := Config{ID: "fixture", BaseURL: original.URL, BaseURLLiteral: true, APIKey: "synthetic", APIKeyLiteral: true}
	models, err := DiscoverModels(t.Context(), cfg, resolver)
	require.NoError(t, err)
	require.Equal(t, "original", models[0].ID)
	require.Equal(t, 1, first)
	require.Zero(t, second)
	require.Zero(t, resolver.calls)
	// Ordinary endpoint sources keep the established resolver behavior.
	httpClient = changed.Client()
	cfg.BaseURLLiteral = false
	models, err = DiscoverModels(t.Context(), cfg, resolver)
	require.NoError(t, err)
	require.Equal(t, "changed", models[0].ID)
	require.Equal(t, 1, second)
	require.Equal(t, 1, resolver.calls)
}
