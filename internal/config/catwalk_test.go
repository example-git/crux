package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestCatwalkSyncInit(t *testing.T) {
	t.Parallel()

	syncer := &catwalkSync{}
	path := filepath.Join(t.TempDir(), "providers.json")
	syncer.Init(path)

	require.True(t, syncer.init.Load())
	require.Equal(t, path, syncer.cache.path)
}

func TestCatwalkSyncGetPanicsBeforeInit(t *testing.T) {
	t.Parallel()

	syncer := &catwalkSync{}
	require.Panics(t, func() {
		_, _ = syncer.Get(t.Context())
	})
}

func TestCatwalkSyncUsesEmbeddedCatalogWithoutCache(t *testing.T) {
	t.Parallel()

	syncer := &catwalkSync{}
	syncer.Init(filepath.Join(t.TempDir(), "providers.json"))

	providers, err := syncer.Get(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, providers)
}

func TestCatwalkSyncUsesCachedCatalog(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "providers.json")
	cached := []catwalk.Provider{{Name: "Cached Provider", ID: "cached", Type: catwalk.TypeOpenAICompat}}
	data, err := json.Marshal(cached)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	syncer := &catwalkSync{}
	syncer.Init(path)

	providers, err := syncer.Get(t.Context())
	require.NoError(t, err)
	require.Equal(t, cached, providers)
}

func TestCatwalkSyncInvalidCacheUsesEmbeddedCatalog(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "providers.json")
	require.NoError(t, os.WriteFile(path, []byte("invalid"), 0o644))

	syncer := &catwalkSync{}
	syncer.Init(path)

	providers, err := syncer.Get(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, providers)
}

func TestCatwalkSyncMemoizesCatalog(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "providers.json")
	first := []catwalk.Provider{{Name: "First", ID: "first", Type: catwalk.TypeOpenAICompat}}
	require.NoError(t, newCache[[]catwalk.Provider](path).Store(first))

	syncer := &catwalkSync{}
	syncer.Init(path)
	providers, err := syncer.Get(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, providers)

	second := []catwalk.Provider{{Name: "Second", ID: "second", Type: catwalk.TypeOpenAICompat}}
	require.NoError(t, newCache[[]catwalk.Provider](path).Store(second))
	providers, err = syncer.Get(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, providers)
}
