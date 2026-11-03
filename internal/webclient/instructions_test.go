package webclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstructionFileCreateAndConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instructions", "AGENTS.md")
	missing, err := readInstructionFile(path)
	require.NoError(t, err)
	require.Equal(t, instructionDocument{Path: path}, missing)

	text := "A literal {{prompt}}\n$HOME remains text.\n"
	saved, err := saveInstructionFile(path, text, missing.Revision)
	require.NoError(t, err)
	require.True(t, saved.Exists)
	require.Equal(t, text, saved.Text)
	require.NotEmpty(t, saved.Revision)
	loaded, err := readInstructionFile(path)
	require.NoError(t, err)
	require.Equal(t, saved, loaded)

	_, err = saveInstructionFile(path, "stale create", missing.Revision)
	require.ErrorIs(t, err, errInstructionFileChanged)
	require.NoError(t, os.WriteFile(path, []byte("external edit"), 0o600))
	_, err = saveInstructionFile(path, "stale update", saved.Revision)
	require.ErrorIs(t, err, errInstructionFileChanged)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "external edit", string(content))
}

func TestInstructionFileEmptyDocumentAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))
	require.NoError(t, os.Chmod(path, 0o640))
	before, err := readInstructionFile(path)
	require.NoError(t, err)
	empty, err := saveInstructionFile(path, "", before.Revision)
	require.NoError(t, err)
	require.True(t, empty.Exists)
	require.Empty(t, empty.Text)
	require.NotEmpty(t, empty.Revision)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	_, err = saveInstructionFile(path, "stale create", "")
	require.ErrorIs(t, err, errInstructionFileChanged)
}

func TestInstructionFileRejectsInvalidText(t *testing.T) {
	for name, text := range map[string]string{"encoding": string([]byte{0xff}), "size": strings.Repeat("x", instructionFileLimit+1)} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "AGENTS.md")
			_, err := saveInstructionFile(path, text, "")
			require.Error(t, err)
			_, err = os.Stat(path)
			require.True(t, os.IsNotExist(err))
			require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
			_, err = readInstructionFile(path)
			require.Error(t, err)
		})
	}
}

func TestInstructionFileRejectsLinksAndDirectories(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.md")
	require.NoError(t, os.WriteFile(target, []byte("unchanged"), 0o600))
	link := filepath.Join(directory, "link.md")
	require.NoError(t, os.Symlink(target, link))
	for _, path := range []string{directory, link} {
		_, err := readInstructionFile(path)
		require.Error(t, err)
		_, err = saveInstructionFile(path, "replacement", "")
		require.Error(t, err)
	}
	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "unchanged", string(content))
}
