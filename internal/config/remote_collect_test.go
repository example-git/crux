package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCollectRemoteRuntimeUsesAcceptedClientBundleGeneration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	store, _, paths, status := setupReloadPluginStore(t)
	before := store.RuntimeSnapshot()
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, proposal.Providers, 1)
	require.Equal(t, "example-echo", proposal.Providers[0].Config.ID)
	require.Len(t, proposal.Credentials, 1)
	require.Equal(t, "test-key", proposal.Credentials[0].APIKey)
	require.Empty(t, proposal.Providers[0].Config.APIKey)
	require.Equal(t, before.Config().Models, proposal.Models)
	require.Len(t, proposal.Bundles, 1)
	require.Equal(t, status.Digest, proposal.Bundles[0].Digest)
	require.NoError(t, os.RemoveAll(filepath.Join(paths.Bundles, status.BundleName)))
	after, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, proposal, after, "accepted export must not reopen replaced or removed paths")
	proposal.Bundles[0].Files[0].Data[0] = '!'
	independent, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, after, independent)
	root := t.TempDir()
	_, err = CompileRemoteRuntime(root, filepath.Join(root, "remote"), false, independent, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SnapshotEnvironment())
	require.NoError(t, err, "collected production client state must compile without installed server bundles")
}
