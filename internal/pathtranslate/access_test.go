package pathtranslate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInternalStateAccessRejectsDatabaseAndAliases(t *testing.T) {
	store, _ := testStore(t)
	_, err := store.Save(t.Context(), Policy{Scope: Scope{Kind: "session", SessionID: "one"}})
	require.NoError(t, err)
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	root := t.TempDir()
	translator, err := New(names, Settings{}, root)
	require.NoError(t, err)
	link := filepath.Join(root, "linked.txt")
	require.NoError(t, os.Symlink(names.databasePath, link))
	hardlink := filepath.Join(root, "hardlinked.txt")
	require.NoError(t, os.Link(names.databasePath, hardlink))
	directory := filepath.Join(root, "state")
	require.NoError(t, os.Symlink(filepath.Dir(names.databasePath), directory))
	for _, path := range []string{names.databasePath, names.databasePath + "-wal", names.databasePath + "-shm", names.databasePath + "-journal", link, hardlink, filepath.Join(directory, "crux.db"), filepath.Join(directory, "crux.db-journal")} {
		require.ErrorIs(t, translator.CheckInternalPath(path), ErrInternalStateAccess, path)
		input, err := json.Marshal(map[string]string{"file_path": path})
		require.NoError(t, err)
		require.ErrorIs(t, translator.CheckToolInput(string(input)), ErrInternalStateAccess, path)
	}
	ordinary := filepath.Join(root, "ordinary.db")
	require.NoError(t, os.WriteFile(ordinary, []byte("ordinary data"), 0o600))
	require.NoError(t, translator.CheckInternalPath(ordinary))
	input, err := json.Marshal(map[string]string{"file_path": ordinary, "content": names.databasePath})
	require.NoError(t, err)
	require.NoError(t, translator.CheckToolInput(string(input)))
}
