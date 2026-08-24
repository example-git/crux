package codebaseindex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectReadsRejectSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.go")
	require.NoError(t, os.WriteFile(target, []byte("package secret\n"), 0o600))
	link := filepath.Join(root, "secret.go")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}

	content, err := readNativeProjectFile(root, nativeProjectFile{Path: "secret.go"})
	require.NoError(t, err)
	require.Empty(t, content)
	_, ok := projectExcerpt(root, "secret.go", 1, 1)
	require.False(t, ok)
}
