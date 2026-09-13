package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func TestRemoteCodebaseIndexCredentialAndPathOwnership(t *testing.T) {
	store, _, _, _ := setupReloadPluginStore(t)
	clientDir, serverDir := os.Getenv("AI_CLI_DIR"), t.TempDir()
	require.NoError(t, os.MkdirAll(clientDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(clientDir, "accounts.json"), []byte(`{"active":{"codebase-index":"client"},"accounts":{"codebase-index":[{"id":"client","displayName":"Client","accessToken":"synthetic-client-codebase"}]}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(serverDir, "accounts.json"), []byte(`{"active":{"codebase-index":"server"},"accounts":{"codebase-index":[{"id":"server","displayName":"Server","accessToken":"synthetic-server-codebase"}]}}`), 0o600))
	require.NoError(t, store.SetConfigFields(ScopeWorkspace, map[string]any{"tools.codebase_search.enabled": true, "tools.codebase_search.database_path": "/client/source.db", "tools.codebase_search.store_directory": "/client/store"}))
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.NotNil(t, proposal.CodebaseIndex)
	require.Empty(t, proposal.CodebaseIndex.Settings.DatabasePath)
	require.Empty(t, proposal.CodebaseIndex.Settings.GetStoreDirectory())
	require.Equal(t, "synthetic-client-codebase", proposal.CodebaseIndex.AccessToken)
	t.Setenv("AI_CLI_DIR", serverDir)
	root, data := t.TempDir(), t.TempDir()
	receiver, err := CompileRemoteRuntime(root, data, false, proposal, strings.Repeat("a", 64), env.New())
	require.NoError(t, err)
	require.True(t, receiver.Config().Tools.CodebaseSearch.IsEnabled())
	require.Equal(t, filepath.Join(data, "codebase-index"), receiver.Config().Tools.CodebaseSearch.StoreDirectory)
	accepted := receiver.RuntimeSnapshot()
	token, err := accepted.CodebaseIndexToken(t.Context())
	require.NoError(t, err)
	require.Equal(t, "synthetic-client-codebase", token)
	receiver.RegisterRemoteRuntimeSecrets()
	require.Equal(t, redact.Replacement, redact.String(token))
	public, err := json.Marshal(receiver.Config().RedactedForTransport())
	require.NoError(t, err)
	require.NotContains(t, string(public), token)
	proposal.Revision = 2
	proposal.CodebaseIndex.AccessToken = ""
	proposal = sealRemoteRuntime(t, proposal)
	_, err = receiver.ReplaceRemoteRuntime(t.Context(), proposal, strings.Repeat("a", 64), 1)
	require.NoError(t, err)
	token, err = receiver.RuntimeSnapshot().CodebaseIndexToken(t.Context())
	require.NoError(t, err)
	require.Empty(t, token, "missing client token must not use the server file")
	token, err = accepted.CodebaseIndexToken(t.Context())
	require.NoError(t, err)
	require.Equal(t, "synthetic-client-codebase", token, "captured operation keeps its exact credential")
	receiver.RevokeRuntime()
	_, err = accepted.CodebaseIndexToken(t.Context())
	require.ErrorIs(t, err, ErrRuntimeRevoked)
}

func TestCodebaseIndexAccountLifecycle(t *testing.T) {
	store, _, _, _ := setupReloadPluginStore(t)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("codebase-index")
	require.True(t, ok)
	models := store.Config().Models
	for _, id := range []string{"first", "second"} {
		require.NoError(t, accounts.SaveWithoutActivating(t.Context(), owner.AccountNamespace, accounts.Entry{ID: id, DisplayName: id, AccessToken: "synthetic-" + id}))
	}
	for _, id := range []string{"first", "second"} {
		before, err := store.CaptureAuthentication(t.Context())
		require.NoError(t, err)
		result, err := store.SwitchAuthenticationAccount(t.Context(), ScopeGlobal, before, owner, id)
		require.NoError(t, err)
		require.True(t, result.RuntimePublished)
		token, err := store.RuntimeSnapshot().CodebaseIndexToken(t.Context())
		require.NoError(t, err)
		require.Equal(t, "synthetic-"+id, token)
		require.NoError(t, store.ReloadFromDisk(t.Context()))
		token, err = store.RuntimeSnapshot().CodebaseIndexToken(t.Context())
		require.NoError(t, err)
		require.Equal(t, "synthetic-"+id, token)
		require.Equal(t, models, store.Config().Models)
	}
	before, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	_, err = store.LogoutAuthentication(t.Context(), ScopeGlobal, before, owner)
	require.NoError(t, err)
	token, err := store.RuntimeSnapshot().CodebaseIndexToken(t.Context())
	require.NoError(t, err)
	require.Empty(t, token)
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	token, err = store.RuntimeSnapshot().CodebaseIndexToken(t.Context())
	require.NoError(t, err)
	require.Empty(t, token)
}

func TestRemoteCodebaseIndexSettingsAreScopedAndSurviveReload(t *testing.T) {
	store, _, _, _ := setupReloadPluginStore(t)
	store.BindRemoteCodebaseIndexScope("first-connection", "/server/project")
	key := RemoteCodebaseIndexScope("first-connection", "/server/project")
	settings := ToolCodebaseSearch{Enabled: new(true), StoreDirectory: "/server/chosen-store", IncludePaths: []string{"src"}}
	require.NoError(t, store.SetConfigFields(ScopeGlobal, map[string]any{"remote_codebase_indexes." + key: settings}))
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, settings, proposal.CodebaseIndex.Settings)
	store.BindRemoteCodebaseIndexScope("second-connection", "/server/project")
	proposal, err = store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Nil(t, proposal.CodebaseIndex, "another connection must not inherit server paths")
}
