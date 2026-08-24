package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestProviderSurfaces(t *testing.T) {
	t.Parallel()
	want := []providerregistry.Surface{{
		ID: "synthetic", Name: "Synthetic", Order: 3,
		Brand:         &providerregistry.Brand{ShortName: "SYNTH", Color: "#123456"},
		Configuration: map[string]any{"type": "object"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/workspaces/workspace%20id/providers", r.URL.EscapedPath())
		require.NoError(t, json.NewEncoder(w).Encode(want))
	}))
	defer srv.Close()

	got, err := captureClient(t, srv).ProviderSurfaces(t.Context(), "workspace id")
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestProviderSurfacesRejectMalformedResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := captureClient(t, srv).ProviderSurfaces(t.Context(), "workspace")
	require.ErrorContains(t, err, "failed to decode workspace provider surfaces")
}
