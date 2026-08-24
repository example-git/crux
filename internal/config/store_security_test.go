package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRefreshLockPathHashesProviderID(t *testing.T) {
	root := t.TempDir()
	store := &ConfigStore{globalDataPath: filepath.Join(root, "crux.json")}

	path := store.refreshLockPath("../../outside")
	require.Equal(t, filepath.Join(root, "locks"), filepath.Dir(path))
	require.Equal(t, ".lock", filepath.Ext(path))
}
