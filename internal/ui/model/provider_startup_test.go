package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type providerStartupWorkspace struct{ testWorkspace }

func (*providerStartupWorkspace) PermissionSkipRequests() bool     { return false }
func (*providerStartupWorkspace) AgentModel() workspace.AgentModel { return workspace.AgentModel{} }

func TestProviderStartupScreenFromLoadedConfig(t *testing.T) {
	root := t.TempDir()
	values := map[string]string{
		"HOME": root, "USERPROFILE": root, "XDG_CONFIG_HOME": filepath.Join(root, "xdg"),
		"AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"),
		"CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"),
		"CRUX_PROVIDER_PROFILE": string(config.ProviderProfilePluginCompat), "CRUX_DISABLE_AUTO_MEMORY": "true",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	workdir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workdir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "crux.json"), []byte(`{"providers":{"codex":{},"gemini-ag":{}}}`), 0o600))
	store, err := config.LoadIsolated(workdir, filepath.Join(root, "workspace-data"), false, env.NewFromMap(values))
	require.NoError(t, err)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Empty(t, proposal.Models)
	require.Empty(t, proposal.Credentials)
	require.Len(t, proposal.Providers, 2)
	remote, err := config.CompileRemoteRuntime(filepath.Join(root, "remote"), filepath.Join(root, "remote-data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	require.False(t, remote.Config().CanInitializeAgent())
	require.Len(t, remote.Config().ProviderLoadIssues(), 2)
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "onboarding", true: "ready"}[ready], func(t *testing.T) {
			ws := &providerStartupWorkspace{testWorkspace: testWorkspace{cfg: store.Config(), ready: ready}}
			m := New(common.DefaultCommon(ws), "", false, "pending prompt")
			m.width, m.height = 100, 36
			m.Init()
			require.True(t, m.providerStartupPending)
			require.True(t, m.dialog.ContainsDialog(dialog.ProviderLoadIssuesID))
			require.False(t, m.dialog.ContainsDialog(dialog.ModelsID))
			require.Nil(t, m.continueStartup())
			require.Equal(t, "pending prompt", m.initialPrompt)
			screen := uv.NewScreenBuffer(100, 36)
			m.dialog.Draw(screen, screen.Bounds())
			view := ansi.Strip(screen.Render())
			for _, text := range []string{"Providers not loaded", "codex", "gemini-ag", "Continue"} {
				require.Contains(t, view, text)
			}
			cmd := m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
			if ready {
				require.NotNil(t, cmd)
			}
			require.False(t, m.providerStartupPending)
			require.False(t, m.dialog.ContainsDialog(dialog.ProviderLoadIssuesID))
			require.Equal(t, !ready, m.dialog.ContainsDialog(dialog.ModelsID))
			if ready {
				require.Empty(t, m.initialPrompt)
			} else {
				require.Equal(t, "pending prompt", m.initialPrompt)
			}
			require.Nil(t, m.handleDialogAction(dialog.ActionContinueProviderStartup{}), "a repeated dismissal must not resume startup twice")
		})
	}
}
