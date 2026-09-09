package config

import (
	"encoding/json"
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
	proposalJSON, err := json.Marshal(proposal)
	require.NoError(t, err)
	afterJSON, err := json.Marshal(after)
	require.NoError(t, err)
	require.Equal(t, proposalJSON, afterJSON, "accepted export must not reopen replaced or removed paths")
	require.True(t, proposal.collectionSource.runtime.SamePublication(after.collectionSource.runtime))
	proposal.Bundles[0].Files[0].Data[0] = '!'
	independent, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	independentJSON, err := json.Marshal(independent)
	require.NoError(t, err)
	require.Equal(t, afterJSON, independentJSON)
	require.True(t, after.collectionSource.runtime.SamePublication(independent.collectionSource.runtime))
	root := t.TempDir()
	_, err = CompileRemoteRuntime(root, filepath.Join(root, "remote"), false, independent, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SnapshotEnvironment())
	require.NoError(t, err, "collected production client state must compile without installed server bundles")
}
