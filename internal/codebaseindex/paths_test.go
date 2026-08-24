package codebaseindex

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultDatabasePath(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	projectRoot := filepath.Join(t.TempDir(), "project")
	require.NoError(t, os.MkdirAll(projectRoot, 0o755))
	absoluteRoot, err := filepath.Abs(projectRoot)
	require.NoError(t, err)
	digest := sha1.Sum([]byte(absoluteRoot))

	path, err := DefaultDatabasePath(projectRoot)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(configDir, "codebase-index", "project-"+hex.EncodeToString(digest[:])[:8]+".db"), path)
}

func TestResolveDatabasePath(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	matchingDatabase := createTestDatabase(t, []testChunk{{
		projectRoot: "/project",
		path:        "main.go",
		embedding:   encodeEmbedding(1, 0),
		model:       "model-a",
	}})
	require.NoError(t, os.Rename(matchingDatabase, filepath.Join(directory, "shared.db")))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "not-a-database.db"), []byte("invalid"), 0o600))

	path, err := ResolveDatabasePath(ctx, "/project", directory)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(directory, "shared.db"), path)

	path, err = ResolveDatabasePath(ctx, "/project", filepath.Join(directory, "shared.db"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(directory, "shared.db"), path)

	_, err = ResolveDatabasePath(ctx, "/missing", directory)
	require.ErrorContains(t, err, "no database")
}

func TestFindImportDatabasePathAllowsNewIndexes(t *testing.T) {
	ctx := context.Background()
	projectRoot := t.TempDir()

	t.Run("missing default database", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		path, found, err := FindImportDatabasePath(ctx, projectRoot, "")
		require.NoError(t, err)
		require.False(t, found)
		require.Empty(t, path)
	})

	t.Run("configured directory has no matching database", func(t *testing.T) {
		directory := t.TempDir()
		path, found, err := FindImportDatabasePath(ctx, projectRoot, directory)
		require.NoError(t, err)
		require.False(t, found)
		require.Empty(t, path)
	})

	t.Run("configured path does not exist", func(t *testing.T) {
		path, found, err := FindImportDatabasePath(ctx, projectRoot, filepath.Join(t.TempDir(), "new.db"))
		require.NoError(t, err)
		require.False(t, found)
		require.Empty(t, path)
	})
}

func TestFindImportDatabasePathFindsCompatibleDatabase(t *testing.T) {
	projectRoot := "/project"
	database := createTestDatabase(t, []testChunk{{
		projectRoot: projectRoot,
		path:        "main.go",
		embedding:   encodeEmbedding(1, 0),
		model:       "model-a",
	}})

	path, found, err := FindImportDatabasePath(context.Background(), projectRoot, database)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, database, path)
}

func TestResolveDatabasePathIgnoresStandaloneCatalogs(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	storeDirectory, err := DefaultStoreDirectory()
	require.NoError(t, err)
	source := createTestDatabase(t, []testChunk{{
		projectRoot: "/project",
		path:        "main.go",
		embedding:   encodeEmbedding(1, 0),
		model:       "model-a",
	}})
	reader, err := OpenWithANNDirectory(context.Background(), source, storeDirectory)
	require.NoError(t, err)
	_, _, err = reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	directory := t.TempDir()
	wrong := createTestDatabase(t, []testChunk{{
		projectRoot: "/wrong",
		path:        "wrong.go",
		embedding:   encodeEmbedding(1, 0),
		model:       "model-a",
	}})
	right := createTestDatabase(t, []testChunk{{
		projectRoot: "/project",
		path:        "right.go",
		embedding:   encodeEmbedding(1, 0),
		model:       "model-a",
	}})
	require.NoError(t, os.Rename(wrong, filepath.Join(directory, "a-wrong.db")))
	require.NoError(t, os.Rename(right, filepath.Join(directory, "b-right.db")))

	resolved, err := ResolveDatabasePath(context.Background(), "/project", directory)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(directory, "b-right.db"), resolved)
}

func TestCodebaseIndexToken(t *testing.T) {
	t.Run("loads VS Code credential", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AI_CLI_DIR", dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "codebase-index-auth.json"), []byte(`{"accessToken":"secret-token","authMode":"vscode"}`), 0o600))

		token, err := CodebaseIndexToken(context.Background())
		require.NoError(t, err)
		require.Equal(t, "secret-token", token)
	})

	t.Run("missing credential", func(t *testing.T) {
		t.Setenv("AI_CLI_DIR", t.TempDir())
		token, err := CodebaseIndexToken(context.Background())
		require.NoError(t, err)
		require.Empty(t, token)
	})

	t.Run("rejects invalid credential", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AI_CLI_DIR", dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "codebase-index-auth.json"), []byte(`{"accessToken":"secret-token","authMode":"copilot-cli"}`), 0o600))

		_, err := CodebaseIndexToken(context.Background())
		require.ErrorContains(t, err, "credential is invalid")
	})

	t.Run("rejects malformed JSON", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AI_CLI_DIR", dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "codebase-index-auth.json"), []byte(`{"accessToken":`), 0o600))

		_, err := CodebaseIndexToken(context.Background())
		require.ErrorContains(t, err, "parse codebase-index credential")
	})
}
