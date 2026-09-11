package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func completionTestRoot(t *testing.T) *cobra.Command {
	t.Helper()
	unexpectedRun := func(*cobra.Command, []string) error {
		t.Fatal("completion invoked an ordinary application command")
		return nil
	}
	root := &cobra.Command{Use: "crux", RunE: unexpectedRun}
	root.PersistentFlags().String("connection", "", "Saved connection")
	root.PersistentFlags().String("cwd", "", "Working directory")
	root.Flags().Bool("continue", false, "Resume the session")
	run := &cobra.Command{Use: "run", RunE: unexpectedRun}
	run.Flags().String("model", "", "Model")
	root.AddCommand(run, &cobra.Command{Use: "session", RunE: unexpectedRun})
	configureShellCompletions(root)
	return root
}

func TestShellCompletionRouting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		handled bool
		want    string
		wantErr bool
	}{
		{name: "generation", args: []string{"completion", "zsh"}, handled: true, want: "compdef _crux-dev crux-dev"},
		{name: "global flags", args: []string{"--connection", "completion", "completion", "bash"}, handled: true, want: "__start_crux-dev"},
		{name: "root flags", args: []string{"__complete", "--cont"}, handled: true, want: "--continue\tResume the session"},
		{name: "no descriptions", args: []string{"__completeNoDesc", "--cont"}, handled: true, want: "--continue\n:4"},
		{name: "install shells", args: []string{"__complete", "completion", "--install", ""}, handled: true, want: "bash\nzsh\nfish\n:4"},
		{name: "subcommand flags", args: []string{"--cwd", "/unused", "__complete", "run", "--mo"}, handled: true, want: "--model\tModel"},
		{name: "unknown shell", args: []string{"completion", "unknown"}, handled: true, wantErr: true},
		{name: "invalid install", args: []string{"completion", "--install", "unknown"}, handled: true, wantErr: true},
		{name: "empty install", args: []string{"completion", "--install="}, handled: true, wantErr: true},
		{name: "missing install value", args: []string{"completion", "--install"}, handled: true, wantErr: true},
		{name: "extra argument", args: []string{"completion", "bash", "extra"}, handled: true, wantErr: true},
		{name: "invalid flag", args: []string{"completion", "zsh", "--invalid"}, handled: true, wantErr: true},
		{name: "ordinary root"},
		{name: "ordinary run", args: []string{"run", "completion"}},
		{name: "flag value", args: []string{"--connection", "completion", "run", "hello"}},
		{name: "delimiter", args: []string{"--", "completion", "zsh"}},
		{name: "prompt delimiter", args: []string{"run", "--", "__complete"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := completionTestRoot(t)
			var out, diagnostic bytes.Buffer
			handled, err := executeShellCompletion(root, "/tmp/crux-dev", tc.args, &out, &diagnostic)
			require.Equal(t, tc.handled, handled)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.want != "" {
				require.Contains(t, out.String(), tc.want)
			}
			if !handled {
				require.Empty(t, out.String())
				require.Empty(t, diagnostic.String())
				require.Equal(t, "crux", root.Name())
			}
		})
	}
}

func TestShellCompletionScriptsUseInvokedName(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		for _, noDescriptions := range []bool{false, true} {
			t.Run(shell+map[bool]string{false: "", true: "/no-descriptions"}[noDescriptions], func(t *testing.T) {
				args := []string{"completion", shell}
				if noDescriptions {
					args = append(args, "--no-descriptions")
				}
				var out, diagnostic bytes.Buffer
				handled, err := executeShellCompletion(completionTestRoot(t), "/somewhere/crux-dev", args, &out, &diagnostic)
				require.True(t, handled)
				require.NoError(t, err)
				require.Empty(t, diagnostic.String())
				require.Contains(t, out.String(), "crux-dev")
				require.NotContains(t, out.String(), "/somewhere/")
				if noDescriptions {
					require.Contains(t, out.String(), "__completeNoDesc")
				}
				if shell == "zsh" {
					require.True(t, strings.HasPrefix(out.String(), "#compdef crux-dev\n"))
					require.Contains(t, out.String(), "if (( ! $+functions[compdef] )); then")
				}
			})
		}
	}
}

func TestShellCompletionInstall(t *testing.T) {
	for _, shell := range []string{"zsh", "bash", "fish"} {
		for _, existing := range []string{"missing", "empty", "no newline", "newline", "already installed", "commented out"} {
			t.Run(shell+"/"+existing, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
				t.Setenv("XDG_CONFIG_HOME", "")
				line, path, err := completionSetup("crux-dev", shell)
				require.NoError(t, err)
				contents := map[string]string{
					"missing": "", "empty": "", "no newline": "# existing settings",
					"newline": "# existing settings\n", "already installed": "# existing settings\n  " + line + " # completions\n",
					"commented out": "# " + line + "\n",
				}[existing]
				var originalMode os.FileMode
				if existing != "missing" {
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
					require.NoError(t, os.WriteFile(path, []byte(contents), 0o640))
					info, err := os.Stat(path)
					require.NoError(t, err)
					originalMode = info.Mode().Perm()
				}
				runInstall := func() string {
					var out, diagnostic bytes.Buffer
					handled, err := executeShellCompletion(completionTestRoot(t), "/bin/crux-dev", []string{"completion", "--install", shell}, &out, &diagnostic)
					require.True(t, handled)
					require.NoError(t, err)
					require.Empty(t, diagnostic.String())
					require.Contains(t, out.String(), path)
					require.Contains(t, out.String(), "Load them in this shell now:\n  "+line)
					return out.String()
				}
				runInstall()
				installed, err := os.ReadFile(path)
				require.NoError(t, err)
				want := contents
				if existing != "already installed" {
					if want != "" && !strings.HasSuffix(want, "\n") {
						want += "\n"
					}
					want += line + "\n"
				}
				require.Equal(t, want, string(installed))
				require.Contains(t, runInstall(), "already configured")
				again, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, installed, again)
				if existing != "missing" {
					info, err := os.Stat(path)
					require.NoError(t, err)
					require.Equal(t, originalMode, info.Mode().Perm())
				}
			})
		}
	}
}

func TestShellCompletionInstallConfigLocationAndErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "custom-config"))
	_, path, err := completionSetup("crux", "fish")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "custom-config", "fish", "config.fish"), path)
	for _, shell := range []string{"", "unknown", "../bash", "powershell"} {
		err := installShellCompletion("crux", shell, &bytes.Buffer{})
		require.ErrorContains(t, err, "unsupported installation shell")
	}
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries)
	// An unreadable/non-file target must surface an error without replacing it.
	require.NoError(t, os.Mkdir(filepath.Join(home, ".zshrc"), 0o700))
	require.ErrorContains(t, installShellCompletion("crux", "zsh", &bytes.Buffer{}), "read shell startup file")
}
