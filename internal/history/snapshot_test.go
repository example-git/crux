package history

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSnapshotStoreRestoresRollingFileStates(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	store, err := newSnapshotStore(t.TempDir(), workspace)
	require.NoError(t, err)

	path := filepath.Join(workspace, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))
	require.NoError(t, store.capture(t.Context(), "session", "message-one", path, "before", true, 0o600))
	require.NoError(t, os.WriteFile(path, []byte("during-one"), 0o600))
	require.NoError(t, store.capture(t.Context(), "session", "message-one", path, "ignored", true, 0o600))

	require.NoError(t, store.capture(t.Context(), "session", "message-two", path, "during-one", true, 0o600))
	require.NoError(t, os.WriteFile(path, []byte("during-two"), 0o600))

	paths, err := store.paths("session", "message-two")
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(store.workingDir, "file.txt")}, paths)

	require.NoError(t, store.rewind(t.Context(), "session", []string{"message-one", "message-two"}, true))
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "before", string(content))
}

func TestSnapshotStoreRestoresExactFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve Unix permission bits")
	}

	for _, mode := range []os.FileMode{0, 0o600} {
		t.Run(mode.String(), func(t *testing.T) {
			workspace := t.TempDir()
			store, err := newSnapshotStore(t.TempDir(), workspace)
			require.NoError(t, err)

			path := filepath.Join(workspace, "file.txt")
			require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))
			require.NoError(t, os.Chmod(path, mode))
			require.NoError(t, store.capture(t.Context(), "session", "message", path, "before", true, mode))
			require.NoError(t, os.Chmod(path, 0o644))
			require.NoError(t, os.WriteFile(path, []byte("after"), 0o644))

			require.NoError(t, store.rewind(t.Context(), "session", []string{"message"}, true))
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, mode, info.Mode().Perm())
			require.NoError(t, os.Chmod(path, 0o600))
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "before", string(content))
		})
	}
}

func TestSnapshotStoreRemovesNewFilesOnRestore(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	store, err := newSnapshotStore(t.TempDir(), workspace)
	require.NoError(t, err)

	path := filepath.Join(workspace, "new", "file.txt")
	require.NoError(t, store.capture(t.Context(), "session", "message", path, "", false, 0))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("created"), 0o644))

	require.NoError(t, store.rewind(t.Context(), "session", []string{"message"}, true))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSnapshotStoreRequiresApprovalForExternalFiles(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	store, err := newSnapshotStore(t.TempDir(), workspace)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "external.txt")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))
	err = store.capture(t.Context(), "session", "message", path, "before", true, 0o600)
	require.ErrorIs(t, err, ErrSnapshotOutsideWorkspace)

	ctx := WithExternalSnapshotApproval(t.Context())
	require.NoError(t, store.capture(ctx, "session", "message", path, "before", true, 0o600))
	require.NoError(t, os.WriteFile(path, []byte("after"), 0o600))
	require.NoError(t, store.rewind(t.Context(), "session", []string{"message"}, true))
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "before", string(content))
}

func TestSnapshotStoreCanDiscardWithoutRestoring(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	store, err := newSnapshotStore(t.TempDir(), workspace)
	require.NoError(t, err)

	path := filepath.Join(workspace, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))
	require.NoError(t, store.capture(t.Context(), "session", "message", path, "before", true, 0o644))
	require.NoError(t, os.WriteFile(path, []byte("after"), 0o644))

	require.NoError(t, store.rewind(t.Context(), "session", []string{"message"}, false))
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "after", string(content))
	paths, err := store.paths("session", "message")
	require.NoError(t, err)
	require.Empty(t, paths)
}

func TestSnapshotStoreCullsOldCheckpoints(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	store, err := newSnapshotStore(t.TempDir(), workspace)
	require.NoError(t, err)

	for index := range snapshotRetention + 3 {
		path := filepath.Join(workspace, "file.txt")
		messageID := string(rune('a' + index))
		require.NoError(t, store.capture(t.Context(), "session", messageID, path, "before", true, 0o644))
	}

	entries, err := os.ReadDir(store.sessionDirectory("session"))
	require.NoError(t, err)
	require.Len(t, entries, snapshotRetention)
}
