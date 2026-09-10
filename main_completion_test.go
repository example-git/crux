package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShellCompletionMainProcess(t *testing.T) {
	if os.Getenv("CRUX_TEST_COMPLETION_MAIN") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"/test/crux-dev"}, os.Args[i+1:]...)
			break
		}
	}
	main()
	loaded := os.Getenv("CRUX_TEST_DOTENV_LOADED") != ""
	if loaded != (os.Getenv("CRUX_TEST_EXPECT_DOTENV") == "1") {
		_, _ = os.Stderr.WriteString("unexpected dotenv loading in main\n")
		os.Exit(2)
	}
	os.Exit(0)
}

func TestShellCompletionMainSkipsApplicationStartup(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"zsh", []string{"completion", "zsh"}, "compdef _crux-dev crux-dev"},
		{"bash", []string{"completion", "bash"}, "__start_crux-dev"},
		{"fish", []string{"completion", "fish"}, "fish completion for crux-dev"},
		{"root flag", []string{"__complete", "--cont"}, "--continue"},
		{"subcommand", []string{"__complete", "session", ""}, "list"},
		{"install choices", []string{"__complete", "completion", "--install", ""}, "fish"},
		{"help", []string{"completion", "--help"}, "crux-dev completion --install zsh"},
		{"install", []string{"completion", "--install", "zsh"}, `eval "$(crux-dev completion zsh)"`},
		{"ordinary help", []string{"--help"}, "A glamorous, terminal-first AI assistant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("AI_CLI_DIR", filepath.Join(home, "ai-cli"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			t.Setenv("CRUX_TEST_COMPLETION_MAIN", "1")
			t.Setenv("CRUX_TEST_EXPECT_DOTENV", "")
			if tc.name == "ordinary help" {
				t.Setenv("CRUX_TEST_EXPECT_DOTENV", "1")
			}
			t.Setenv("CRUX_TEST_DOTENV_LOADED", "")
			require.NoError(t, os.Unsetenv("CRUX_TEST_DOTENV_LOADED"))
			require.NoError(t, os.WriteFile(filepath.Join(home, ".env"), []byte("CRUX_TEST_DOTENV_LOADED=1\n"), 0o600))
			args := append([]string{"-test.run=^TestShellCompletionMainProcess$", "--"}, tc.args...)
			command := exec.Command(executable, args...)
			command.Dir = home
			var out, diagnostic bytes.Buffer
			command.Stdout, command.Stderr = &out, &diagnostic
			require.NoError(t, command.Run(), diagnostic.String())
			require.Contains(t, out.String(), tc.want)
			entries, err := os.ReadDir(home)
			require.NoError(t, err)
			if tc.name == "install" {
				require.Len(t, entries, 2)
				contents, err := os.ReadFile(filepath.Join(home, ".zshrc"))
				require.NoError(t, err)
				require.Equal(t, "eval \"$(crux-dev completion zsh)\"\n", string(contents))
			} else {
				require.Len(t, entries, 1)
			}
		})
	}
}

func TestShellCompletionBashWithoutCompletionPackage(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASH_ENV", "")
	t.Setenv("CRUX_TEST_COMPLETION_MAIN", "1")
	t.Setenv("CRUX_TEST_EXPECT_DOTENV", "")
	t.Setenv("CRUX_TEST_DOTENV_LOADED", "")
	t.Setenv("CRUX_TEST_BINARY", executable)
	// The shell wrapper invokes the real main/command tree in a fresh process
	// for generation and every completion request, using the renamed command.
	command := exec.Command(bash, "--noprofile", "--norc", "-c", `
crux-dev() { "$CRUX_TEST_BINARY" -test.run='^TestShellCompletionMainProcess$' -- "$@"; }
eval "$(crux-dev completion bash)"
COMP_WORDS=(crux-dev --cont); COMP_CWORD=1
COMP_LINE='crux-dev --cont'; COMP_POINT=${#COMP_LINE}
__start_crux-dev
[[ ${COMPREPLY[0]} == --continue ]] || exit 10
COMP_WORDS=(crux-dev completion --install = zs); COMP_CWORD=4
COMP_LINE='crux-dev completion --install=zs'; COMP_POINT=${#COMP_LINE}
__start_crux-dev
[[ ${COMPREPLY[0]} == zsh ]] || exit 11
COMP_WORDS=(crux-dev --host tcp : //192.168.1.117 : 14995 run --mo); COMP_CWORD=8
COMP_LINE='crux-dev --host tcp://192.168.1.117:14995 run --mo'; COMP_POINT=${#COMP_LINE}
__start_crux-dev
[[ ${COMPREPLY[0]} == --model* ]] || exit 12
`)
	command.Dir = home
	var out, diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &out, &diagnostic
	require.NoError(t, command.Run(), diagnostic.String())
	require.Empty(t, diagnostic.String())
}
