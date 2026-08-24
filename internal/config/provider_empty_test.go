package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"github.com/stretchr/testify/require"
)

func TestUpdateProvidersDefaultsToEmbedded(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	require.NoError(t, UpdateProviders(""))
	providers, _, err := newCache[[]catwalk.Provider](cachePathFor("providers")).Get()
	require.NoError(t, err)
	require.Equal(t, retainedCatalogProviders(embedded.GetAll()), providers)
}

func TestUpdateProvidersFromExplicitFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	want := []catwalk.Provider{{ID: "local", Name: "Local", Type: catwalk.TypeOpenAICompat}}
	data, err := json.Marshal(want)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "providers.json")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	require.NoError(t, UpdateProviders(path))
	got, _, err := newCache[[]catwalk.Provider](cachePathFor("providers")).Get()
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestUpdateProvidersFromExplicitURL(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	want := []catwalk.Provider{{ID: "remote", Name: "Remote", Type: catwalk.TypeOpenAICompat}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/providers", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(want))
	}))
	defer server.Close()

	require.NoError(t, UpdateProviders(server.URL))
	got, _, err := newCache[[]catwalk.Provider](cachePathFor("providers")).Get()
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestUpdateProvidersRejectsInvalidSource(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	err := UpdateProviders(filepath.Join(t.TempDir(), "missing.json"))
	require.ErrorContains(t, err, "failed to read file")
}
