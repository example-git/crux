package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/compatibility/install"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func executeAliasTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := newAliasCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetArgs(args)
	err := command.ExecuteContext(t.Context())
	return out.String(), err
}

func TestAliasCommandRejectsInvalidSelectionWithoutMutation(t *testing.T) {
	for _, operation := range []string{"install", "uninstall"} {
		for _, selection := range [][]string{nil, {"unknown"}, {"codex", "claude"}, {"--all", "codex"}, {"--all=false"}, {"--all=invalid"}} {
			root := filepath.Join(t.TempDir(), "aliases")
			args := append([]string{operation, "--root", root}, selection...)
			_, err := executeAliasTest(t, args...)
			require.Error(t, err, args)
			_, err = os.Stat(root)
			require.ErrorIs(t, err, os.ErrNotExist)
		}
	}
}

func TestAliasCommandSelectsProviders(t *testing.T) {
	for provider, expected := range map[string]string{
		"codex": "codex", "claude": "claude", "claude-ai": "claude", "antigravity": "agy", "gemini-ag": "agy", "agy": "agy", "copilot": "copilot",
	} {
		command, _, err := newAliasCommand().Find([]string{"install"})
		require.NoError(t, err)
		names, err := aliasSelection(command, []string{provider})
		require.NoError(t, err)
		require.Equal(t, []string{expected}, names)
	}
	command, _, err := newAliasCommand().Find([]string{"install"})
	require.NoError(t, err)
	require.NoError(t, command.Flags().Set("all", "true"))
	names, err := aliasSelection(command, nil)
	require.NoError(t, err)
	require.Equal(t, install.CanonicalAliases(), names)
}

func TestAliasCommandUninstallsOnlySelectedProvider(t *testing.T) {
	root := filepath.Join(t.TempDir(), "aliases")
	executable, err := os.Executable()
	require.NoError(t, err)
	manager, err := install.New(root)
	require.NoError(t, err)
	_, err = manager.Install(install.Options{Executable: executable, Aliases: []string{"codex", "agy"}, SkipPath: true})
	require.NoError(t, err)
	out, err := executeAliasTest(t, "uninstall", "antigravity", "--root", root)
	require.NoError(t, err)
	require.Contains(t, out, "installed: true")
	require.Contains(t, out, "codex: linked")
	require.Contains(t, out, "agy: missing")
	_, err = os.Stat(filepath.Join(root, "bin", "codex"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "bin", "agy"))
	require.ErrorIs(t, err, os.ErrNotExist)
	out, err = executeAliasTest(t, "uninstall", "--all", "--root", root)
	require.NoError(t, err)
	require.Contains(t, out, "installed: false")
}

func TestAliasCommandPropagatesManagerFailures(t *testing.T) {
	root := filepath.Join(t.TempDir(), "aliases")
	_, err := executeAliasTest(t, "repair", "--root", root, "--skip-path")
	require.ErrorContains(t, err, "not installed")
	require.NoError(t, os.WriteFile(root, []byte("collision"), 0o600))
	_, err = executeAliasTest(t, "install", "codex", "--root", root, "--skip-path")
	require.ErrorContains(t, err, "not a directory")
	data, err := os.ReadFile(root)
	require.NoError(t, err)
	require.Equal(t, "collision", string(data))
}

func TestAliasCommandCompletionAndRegistration(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"alias", "install"})
	require.NoError(t, err)
	require.Equal(t, "crux alias install", command.CommandPath())
	root := &cobra.Command{Use: "crux"}
	root.AddCommand(newAliasCommand())
	var out, stderr bytes.Buffer
	handled, err := executeShellCompletion(root, "crux", []string{"__complete", "alias", "install", ""}, &out, &stderr)
	require.True(t, handled)
	require.NoError(t, err)
	for _, provider := range []string{"codex", "claude", "antigravity", "agy", "copilot"} {
		require.Contains(t, out.String(), provider+"\n")
	}
}
