package useragent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestVersionProbeRejectsCurrentExecutable(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	alias := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.Link(executable, alias))
	for _, ctx := range []context.Context{t.Context(), oauth.ContextWithEnvironment(t.Context(), []string{})} {
		_, err := commandOutputForContext(ctx, alias, "--version")
		require.ErrorContains(t, err, "cannot probe the current executable")
	}
}
