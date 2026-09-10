package cmd

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/server"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestRemoteCollectionIgnoresLaunchDirectoryModelConfig(t *testing.T) {
	for _, key := range []string{"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	t.Setenv("CRUX_PROVIDER_PROFILE", "integrated")
	global := []byte(`{"providers":{"client-global":{"type":"openai-compat","api_key":"synthetic-global","base_url":"https://global.invalid/v1","models":[{"id":"global-model","name":"Global"}]}},"models":{"large":{"provider":"client-global","model":"global-model"},"small":{"provider":"client-global","model":"global-model"}}}`)
	if os.Getenv("CRUX_TEST_SHORT_REMOTE_CREDENTIAL") == "1" {
		global = bytes.ReplaceAll(global, []byte("synthetic-global"), []byte("1"))
	}
	require.NoError(t, os.WriteFile(config.GlobalConfigData(), global, 0600))
	root := t.TempDir()
	t.Chdir(root)
	project := []byte(`{"providers":{"client-project":{"type":"openai-compat","api_key":"synthetic-project","base_url":"https://project.invalid/v1","models":[{"id":"project-model","name":"Project"}]}},"models":{"large":{"provider":"client-project","model":"project-model"},"small":{"provider":"client-project","model":"project-model"}}}`)
	projectPath := filepath.Join(root, "crux.json")
	require.NoError(t, os.WriteFile(projectPath, project, 0600))
	workspacePath := filepath.Join(root, ".crux", "crux.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(workspacePath), 0700))
	require.NoError(t, os.WriteFile(workspacePath, project, 0600))
	remotePath, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	s := server.NewServer(nil, "tcp", "127.0.0.1:0")
	defer s.Backend().Shutdown()
	defer s.Close()
	require.NoError(t, s.SetWorkspaceRoots([]string{remotePath}))
	remote := httptest.NewUnstartedServer(s.Handler())
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	_, clientCode, err := connection.Add(t.Context(), "remote-config-test", "tcp://"+remote.Listener.Addr().String(), serverCode)
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "remote-config-test", clientCode))
	require.NoError(t, s.EnableNetworkAuth(t.Context()))
	remote.TLS, err = connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	remote.StartTLS()
	defer remote.Close()
	previousConnection := connectionName
	connectionName = "remote-config-test"
	defer func() { connectionName = previousConnection }()
	command := &cobra.Command{Use: "remote"}
	command.SetContext(t.Context())
	command.Flags().String("cwd", remotePath, "")
	command.Flags().String("data-dir", "", "")
	command.Flags().Bool("debug", false, "")
	command.Flags().Bool("yolo", false, "")
	command.Flags().StringSlice("channels", nil, "")
	c, ws, cleanup, err := connectToServer(command)
	require.NoError(t, err)
	defer cleanup()
	require.Equal(t, remotePath, ws.Path)
	require.Equal(t, "client", ws.Authority.Mode)
	proposal, err := c.LocalRuntimeStore().CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	for _, role := range []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall} {
		require.Equal(t, "client-global", proposal.Models[role].Provider)
		require.Equal(t, "global-model", proposal.Models[role].Model)
	}
	require.Len(t, proposal.Providers, 1)
	require.Equal(t, "client-global", proposal.Providers[0].Config.ID)
	require.Equal(t, proposal.Digest, ws.Authority.Digest)
	opened, err := c.GetWorkspace(t.Context(), ws.ID)
	require.NoError(t, err)
	require.Equal(t, ws.Authority, opened.Authority)
	listed, err := c.ListWorkspaces(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, ws.ID, listed[0].ID)
	require.Equal(t, ws.Authority, listed[0].Authority)
	require.NoError(t, c.LocalRuntimeStore().ReloadFromDisk(t.Context()))
	reloaded, err := c.LocalRuntimeStore().CollectRemoteRuntime(t.Context(), 2)
	require.NoError(t, err)
	require.Equal(t, proposal.Models, reloaded.Models)
	accepted, err := c.ReplaceRemoteRuntime(t.Context(), ws.ID, 1, reloaded)
	require.NoError(t, err)
	require.Equal(t, uint64(2), accepted.Revision)
	require.Equal(t, reloaded.Digest, accepted.Digest)
	require.Equal(t, ws.Authority.Principal, accepted.Principal)
	local, err := config.Load(root, "", false)
	require.NoError(t, err)
	require.Equal(t, "client-project", local.Config().Models[config.SelectedModelTypeLarge].Provider)
	for _, path := range []string{projectPath, workspacePath} {
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, project, after)
	}
}

func TestRemoteConnectionWithShortCredential(t *testing.T) {
	// Low-entropy redaction fingerprints outlive their workspace. Isolate
	// this credential from unrelated tests while exercising the real command.
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRemoteCollectionIgnoresLaunchDirectoryModelConfig$")
	command.Env = append(os.Environ(), "CRUX_TEST_SHORT_REMOTE_CREDENTIAL=1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestCollectedRuntimeIncludesOnlySelectedCanonicalAccount(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CRUX_PROVIDER_PROFILE", "integrated")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	entry := accounts.Entry{ID: "forwarded", DisplayName: "Forwarded", AccessToken: "account-secret", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, entry))
	dataDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("CRUX_GLOBAL_CONFIG", t.TempDir())
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "crux.json"),
		[]byte(`{"providers":{"codex":{"api_key":"account-secret","owner":{"type":"core","construction":"integrated-codex"},"models":[{"id":"fixture-model","name":"Fixture"}]}},"models":{"large":{"provider":"codex","model":"fixture-model"},"small":{"provider":"codex","model":"fixture-model"}}}`),
		0o600,
	))

	proposal, err := collectRemoteProviderState(t.Context(), false, 1)
	require.NoError(t, err)
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("codex")
	require.True(t, ok)
	owner := registration.Owner()
	require.Equal(t, accounts.ProviderCodex, owner.AccountNamespace)
	require.Len(t, proposal.Credentials, 1)
	require.Equal(t, owner, proposal.Credentials[0].Owner)
	require.Equal(t, &entry, proposal.Credentials[0].Account)

	require.NoError(t, accounts.Save(t.Context(), "unknown.accounts", accounts.Entry{ID: "unknown", AccessToken: "unknown-secret"}))
	proposal, err = collectRemoteProviderState(t.Context(), false, 2)
	require.NoError(t, err)
	data, err := json.Marshal(proposal)
	require.NoError(t, err)
	require.NotContains(t, string(data), "unknown-secret")
	require.Len(t, proposal.Credentials, 1)
	require.Len(t, proposal.Providers, 1)
	require.Equal(t, "codex", proposal.Models[config.SelectedModelTypeLarge].Provider)
	changed := accounts.Entry{ID: "changed", AccessToken: "different-active-token"}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, changed))
	require.NoError(t, accounts.SetActive(t.Context(), accounts.ProviderCodex, changed.ID))
	_, err = collectRemoteProviderState(t.Context(), false, 3)
	require.ErrorContains(t, err, "selected client account changed")
}
