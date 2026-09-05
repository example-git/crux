package main

import (
	"bytes"
	"testing"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/stretchr/testify/require"
)

func TestDefaultBuildRegistersCompatibilityAliasesWithoutChangingCrux(t *testing.T) {
	t.Setenv(compatibility.BypassEnvironment, "")
	require.NoError(t, registerCompatibility())

	var stdout bytes.Buffer
	exitCode, handled := compatibility.Dispatch(t.Context(), compatibility.Invocation{
		Executable: "/private/compat/bin/codex",
		Args:       []string{"--version"},
		WorkingDir: t.TempDir(),
		Stdout:     &stdout,
		Stderr:     &bytes.Buffer{},
	})
	require.True(t, handled)
	require.Zero(t, exitCode)
	require.Equal(t, "codex-cli 0.149.1\n", stdout.String())

	exitCode, handled = compatibility.Dispatch(t.Context(), compatibility.Invocation{
		Executable: "/usr/local/bin/crux",
		WorkingDir: t.TempDir(),
	})
	require.False(t, handled)
	require.Zero(t, exitCode)
}
