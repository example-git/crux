package providerplugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateGitURL(t *testing.T) {
	valid, err := validateGitURL("https://example.invalid/owner/plugin.git")
	require.NoError(t, err)
	require.Equal(t, "https://example.invalid/owner/plugin.git", valid)

	for _, source := range []string{
		"http://example.invalid/plugin.git",
		"file:///tmp/plugin",
		"ssh://example.invalid/plugin.git",
		"https://user:secret@example.invalid/plugin.git",
		"https://example.invalid:8443/plugin.git",
		"https://example.invalid/plugin.git?token=secret",
		"https://example.invalid/plugin.git#main",
	} {
		t.Run(source, func(t *testing.T) {
			_, err := validateGitURL(source)
			require.Error(t, err)
		})
	}
}

func TestRejectUnsupportedGitMetadata(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs\n"), 0o600))
	require.ErrorContains(t, rejectUnsupportedGitMetadata(root), "Git LFS")

	require.NoError(t, os.Remove(filepath.Join(root, ".gitattributes")))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitmodules"), []byte("[submodule \"x\"]\n"), 0o600))
	require.ErrorContains(t, rejectUnsupportedGitMetadata(root), "submodules")
}
