package install

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSelectedAliasLifecyclePreservesOthersAndPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "aliases")
	profile := filepath.Join(t.TempDir(), ".zshrc")
	require.NoError(t, os.WriteFile(profile, []byte("existing\n"), 0o600))
	executable := testExecutable(t, "crux", "crux")
	manager, err := New(root)
	require.NoError(t, err)
	options := Options{Executable: executable, Aliases: []string{"codex"}, Shell: "zsh", Profile: profile}
	status, err := manager.Install(options)
	require.NoError(t, err)
	require.True(t, status.Installed)
	require.True(t, status.PathSetup)
	for _, name := range []string{"claude", "agy", "copilot"} {
		_, err := os.Stat(filepath.Join(root, "bin", name))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	codexInfo, err := os.Stat(filepath.Join(root, "bin", "codex"))
	require.NoError(t, err)
	profileBefore, err := os.ReadFile(profile)
	require.NoError(t, err)
	options.Aliases = []string{"claude"}
	_, err = manager.Install(options)
	require.NoError(t, err)
	_, err = manager.Install(options)
	require.NoError(t, err)
	current, err := manager.loadState()
	require.NoError(t, err)
	require.Equal(t, []string{"codex", "claude"}, current.aliasNames())
	codexAfter, err := os.Stat(filepath.Join(root, "bin", "codex"))
	require.NoError(t, err)
	require.True(t, os.SameFile(codexInfo, codexAfter))
	_, err = manager.Disable()
	require.NoError(t, err)
	_, err = manager.Enable()
	require.NoError(t, err)
	status, err = manager.Uninstall("codex")
	require.NoError(t, err)
	require.True(t, status.Installed)
	require.True(t, status.PathSetup)
	_, err = os.Stat(filepath.Join(root, "bin", "codex"))
	require.ErrorIs(t, err, os.ErrNotExist)
	profileAfter, err := os.ReadFile(profile)
	require.NoError(t, err)
	require.Equal(t, profileBefore, profileAfter)
	_, err = manager.Uninstall("codex")
	require.NoError(t, err)
	status, err = manager.Repair(Options{})
	require.NoError(t, err)
	require.True(t, status.Installed)
	_, err = os.Stat(filepath.Join(root, "bin", "codex"))
	require.ErrorIs(t, err, os.ErrNotExist)
	status, err = manager.Uninstall("claude")
	require.NoError(t, err)
	require.False(t, status.Installed)
	profileAfter, err = os.ReadFile(profile)
	require.NoError(t, err)
	require.Equal(t, "existing\n", string(profileAfter))
	_, err = os.Stat(manager.statePath())
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSelectedAliasRepairPreservesSubsetAcrossReplacement(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "aliases"))
	require.NoError(t, err)
	executable := testExecutable(t, "crux", "original")
	_, err = manager.Install(Options{Executable: executable, Aliases: []string{"agy"}, SkipPath: true})
	require.NoError(t, err)
	replacement := testExecutable(t, "replacement", "updated")
	require.NoError(t, os.Rename(replacement, executable))
	_, err = manager.Install(Options{Executable: executable, Aliases: []string{"copilot"}, SkipPath: true})
	require.ErrorContains(t, err, "needs repair")
	_, err = os.Stat(filepath.Join(manager.binDirectory(), "copilot"))
	require.ErrorIs(t, err, os.ErrNotExist)
	status, err := manager.Repair(Options{SkipPath: true})
	require.NoError(t, err)
	require.True(t, status.Installed)
	for _, name := range []string{"codex", "claude", "copilot"} {
		_, err := os.Stat(filepath.Join(manager.binDirectory(), name))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	_, err = manager.Install(Options{Executable: executable, SkipPath: true})
	require.NoError(t, err)
	current, err := manager.loadState()
	require.NoError(t, err)
	require.Equal(t, CanonicalAliases(), current.aliasNames())
	_, err = manager.Uninstall()
	require.NoError(t, err)
}

func TestSelectedAliasUninstallRollsBackStateFailure(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "aliases"))
	require.NoError(t, err)
	executable := testExecutable(t, "crux", "crux")
	_, err = manager.Install(Options{Executable: executable, Aliases: []string{"codex", "claude"}, SkipPath: true})
	require.NoError(t, err)
	before, err := os.ReadFile(manager.statePath())
	require.NoError(t, err)
	manager.failForTest = func(operation string) error {
		if operation == operationSaveState {
			return errors.New("injected state failure")
		}
		return nil
	}
	_, err = manager.Uninstall("codex")
	require.ErrorContains(t, err, "injected state failure")
	after, err := os.ReadFile(manager.statePath())
	require.NoError(t, err)
	require.Equal(t, before, after)
	original, err := os.Stat(executable)
	require.NoError(t, err)
	for _, name := range []string{"codex", "claude"} {
		info, err := os.Stat(filepath.Join(manager.binDirectory(), name))
		require.NoError(t, err)
		require.True(t, os.SameFile(original, info))
	}
}

func TestSelectedAliasUninstallDoesNotTouchOtherCollision(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "aliases"))
	require.NoError(t, err)
	executable := testExecutable(t, "crux", "crux")
	_, err = manager.Install(Options{Executable: executable, Aliases: []string{"codex", "claude"}, SkipPath: true})
	require.NoError(t, err)
	collision := filepath.Join(manager.binDirectory(), "claude")
	require.NoError(t, os.Remove(collision))
	require.NoError(t, os.WriteFile(collision, []byte("unrelated"), 0o700))
	_, err = manager.Uninstall("claude")
	require.ErrorContains(t, err, "unrecognized file")
	_, err = os.Stat(filepath.Join(manager.binDirectory(), "codex"))
	require.NoError(t, err)
	_, err = manager.Uninstall("codex")
	require.NoError(t, err)
	data, err := os.ReadFile(collision)
	require.NoError(t, err)
	require.Equal(t, "unrelated", string(data))
}

func TestAliasSelectionFailsBeforeMutation(t *testing.T) {
	for _, names := range [][]string{{}, {"unknown"}, {"../codex"}} {
		manager, err := New(filepath.Join(t.TempDir(), "aliases"))
		require.NoError(t, err)
		_, err = manager.Install(Options{Executable: testExecutable(t, "crux", "crux"), Aliases: names, SkipPath: true})
		require.Error(t, err)
		_, err = manager.Uninstall(names...)
		require.Error(t, err)
		_, err = os.Stat(manager.root)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	manager, err := New(filepath.Join(t.TempDir(), "aliases"))
	require.NoError(t, err)
	_, err = manager.Repair(Options{})
	require.ErrorContains(t, err, "not installed")
	_, err = os.Stat(manager.root)
	require.ErrorIs(t, err, os.ErrNotExist)
}
