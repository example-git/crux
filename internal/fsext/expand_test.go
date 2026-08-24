package fsext

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExpandDoesNotEnumerateGlobs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_EXPAND_ROOT", root)

	expanded, err := Expand("$CRUX_EXPAND_ROOT/*.txt")
	require.NoError(t, err)
	require.Equal(t, filepath.Clean(root)+"/*.txt", expanded)
}
