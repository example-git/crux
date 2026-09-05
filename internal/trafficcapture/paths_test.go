package trafficcapture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsurePrivateDirectoryRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	target := t.TempDir()
	require.NoError(t, os.Chmod(target, 0o755))
	link := filepath.Join(t.TempDir(), "private")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := ensurePrivateDirectory(link)
	require.ErrorContains(t, err, "is not a directory")
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}
