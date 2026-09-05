package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildScriptOptionalTags(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("The build script requires a Unix shell")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash is not installed")
	}
	script, err := filepath.Abs("build.sh")
	require.NoError(t, err)

	for _, mode := range []struct {
		flag string
		args string
	}{
		{flag: "--build", args: "go <build> <-v>"},
		{flag: "--test", args: "go <test> <-race> <-failfast>"},
		{flag: "--check", args: "go <build> <-race>"},
	} {
		t.Run(mode.flag, func(t *testing.T) {
			t.Parallel()
			for _, embedded := range []bool{false, true} {
				name := "untagged"
				if embedded {
					name = "embedded"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					directory := t.TempDir()
					require.NoError(t, os.Mkdir(filepath.Join(directory, "scripts"), 0o700))
					require.NoError(t, os.WriteFile(filepath.Join(directory, "scripts", "check_log_capitalization.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700))
					args := []string{"-c", `go() { printf 'go'; printf ' <%s>' "$@"; printf '\n'; }
python3() { printf 'prepare\n'; }
export -f go python3
source "$@"`, "build-script-test", script, mode.flag}
					if embedded {
						args = append(args, "--embedded-mitmproxy")
					}
					command := exec.CommandContext(t.Context(), bash, args...)
					command.Dir = directory
					output, err := command.CombinedOutput()
					require.NoError(t, err, "%s", output)
					require.Contains(t, string(output), mode.args)
					if embedded {
						require.Contains(t, string(output), mode.args+" <-tags> <embedded_mitmproxy>")
						require.Contains(t, string(output), "prepare\n")
					} else {
						require.NotContains(t, string(output), "<-tags>")
						require.NotContains(t, string(output), "prepare\n")
					}
				})
			}
		})
	}
}

func TestBuildScriptPreservesFailureStatus(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("The build script requires a Unix shell")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash is not installed")
	}
	script, err := filepath.Abs("build.sh")
	require.NoError(t, err)
	command := exec.CommandContext(t.Context(), bash, "-c", `go() { return 19; }
export -f go
source "$@"`, "build-script-test", script, "--build")
	command.Dir = t.TempDir()
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	require.ErrorAs(t, err, &exitError)
	require.Equal(t, 19, exitError.ExitCode())
	require.Contains(t, string(output), "FAILURE: Building Crux exited with status 19.")
	require.NotContains(t, string(output), "SUCCESS:")
}
