package fsext

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLookupBoundedWithCreatedFileMatchesActualOrder(t *testing.T) {
	for _, targetDir := range []string{"root", "child", "outside"} {
		t.Run(targetDir, func(t *testing.T) {
			root := t.TempDir()
			boundary := filepath.Join(root, "root")
			child := filepath.Join(boundary, "child")
			outside := filepath.Join(root, "outside")
			for _, dir := range []string{boundary, child, outside} {
				require.NoError(t, os.MkdirAll(dir, 0o700))
				for _, name := range []string{".cruxrc", "cruxrc", ".crux.json"} {
					require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("retained"), 0o600))
				}
			}
			path := filepath.Join(map[string]string{"root": boundary, "child": child, "outside": outside}[targetDir], "crux.json")
			targets := []string{".cruxrc", "cruxrc", ".crux.json", "crux.json"}
			before, err := LookupBounded(child, boundary, targets...)
			require.NoError(t, err)
			current, projected, err := LookupBoundedWithCreatedFile(t.Context(), child, boundary, path, targets...)
			require.NoError(t, err)
			require.Equal(t, before, current)
			require.NoFileExists(t, path)
			require.NoError(t, os.WriteFile(path, []byte("authored"), 0o600))
			after, err := LookupBounded(child, boundary, targets...)
			require.NoError(t, err)
			require.Equal(t, after, projected)
			if targetDir == "outside" {
				require.Equal(t, current, projected)
			}
		})
	}
}

func TestLookupBoundedWithCreatedFileSymlinksAndRenameEntry(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	linkDir := filepath.Join(root, "link")
	require.NoError(t, os.Mkdir(realDir, 0o700))
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	path := filepath.Join(realDir, "crux.json")
	alias := filepath.Join(linkDir, ".crux.json")
	require.NoError(t, os.Symlink("crux.json", alias))
	current, projected, err := LookupBoundedWithCreatedFile(t.Context(), linkDir, realDir, path, ".crux.json", "crux.json")
	require.NoError(t, err)
	require.Empty(t, current)
	require.Equal(t, []string{alias, filepath.Join(linkDir, "crux.json")}, projected)
	require.NoError(t, os.WriteFile(path, []byte("created"), 0o600))
	after, err := LookupBounded(linkDir, realDir, ".crux.json", "crux.json")
	require.NoError(t, err)
	require.Equal(t, projected, after)

	other := filepath.Join(realDir, "other.json")
	require.NoError(t, os.WriteFile(other, []byte("unrelated"), 0o600))
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(other, path))
	written, err := PathsReadingCreatedFile(t.Context(), path, []string{path, alias, filepath.Join(linkDir, "crux.json"), other})
	require.NoError(t, err)
	require.Equal(t, []string{path, alias, filepath.Join(linkDir, "crux.json")}, written)
	temporary := filepath.Join(realDir, "replacement")
	require.NoError(t, os.WriteFile(temporary, []byte("replacement"), 0o600))
	require.NoError(t, os.Rename(temporary, path))
	for _, writtenPath := range written {
		data, err := os.ReadFile(writtenPath)
		require.NoError(t, err)
		require.Equal(t, "replacement", string(data))
	}
	data, err := os.ReadFile(other)
	require.NoError(t, err)
	require.Equal(t, "unrelated", string(data))
}

func TestLookupBoundedWithCreatedFileCanceledAndMissingParent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "missing", "nested", "crux.json")
	written, err := PathsReadingCreatedFile(t.Context(), path, []string{path, filepath.Join(root, "other", "crux.json")})
	require.NoError(t, err)
	require.Equal(t, []string{path}, written)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = LookupBoundedWithCreatedFile(ctx, root, root, path, "crux.json")
	require.ErrorIs(t, err, context.Canceled)
	_, err = PathsReadingCreatedFile(ctx, path, []string{path})
	require.ErrorIs(t, err, context.Canceled)
	require.NoDirExists(t, filepath.Dir(path))
}
