package codebaseindex

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/stretchr/testify/require"
)

func writePruneCheckpoint(t *testing.T, storeDirectory, name, projectRoot string, updatedAt time.Time) string {
	t.Helper()
	directory := filepath.Join(storeDirectory, name)
	require.NoError(t, os.MkdirAll(directory, 0o700))
	require.NoError(t, writeJSONAtomically(filepath.Join(directory, "migration.json"), storeCatalog{
		Version:           storeCatalogVersion,
		ProjectRoot:       projectRoot,
		Model:             "model-a",
		Directory:         directory,
		Source:            storeSource{Mode: "native"},
		ProgressUpdatedAt: updatedAt,
	}))
	return directory
}

func TestPruneStoreKeepsActiveAndNewestIncompleteGeneration(t *testing.T) {
	path := createTestDatabase(t, []testChunk{{
		projectRoot: "/project",
		path:        "src/main.go",
		embedding:   encodeEmbedding(1, 0),
		model:       "model-a",
	}})
	storeDirectory := t.TempDir()
	firstReader, err := OpenWithANNDirectory(t.Context(), path, storeDirectory)
	require.NoError(t, err)
	_, firstCatalog, err := firstReader.prepareStore(t.Context(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, firstReader.Close())

	secondReader, err := OpenWithFilters(t.Context(), path, storeDirectory, ProjectFilters{IncludePaths: []string{"src"}})
	require.NoError(t, err)
	_, activeCatalog, err := secondReader.prepareStore(t.Context(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, secondReader.Close())
	require.NotEqual(t, firstCatalog.Directory, activeCatalog.Directory)

	olderIncomplete := writePruneCheckpoint(t, storeDirectory, "generation-incomplete-old", "/project", time.Now().Add(-2*time.Hour))
	newerIncomplete := writePruneCheckpoint(t, storeDirectory, "generation-incomplete-new", "/project", time.Now().Add(-time.Hour))
	unknown := filepath.Join(storeDirectory, "generation-unknown")
	require.NoError(t, os.MkdirAll(unknown, 0o700))
	require.NoError(t, writeJSONAtomically(filepath.Join(unknown, "not-a-checkpoint.json"), map[string]any{"unknown": true}))

	result, err := pruneStoreGenerations(storeDirectory)
	require.NoError(t, err)
	require.Equal(t, 2, result.RemovedGenerations)
	require.Equal(t, 2, result.RetainedGenerations)
	require.DirExists(t, activeCatalog.Directory)
	require.NoDirExists(t, firstCatalog.Directory)
	require.NoDirExists(t, olderIncomplete)
	require.DirExists(t, newerIncomplete)
	require.DirExists(t, unknown)
}

func TestPruneStoreKeepsGenerationLeasedByReader(t *testing.T) {
	path := createTestDatabase(t, []testChunk{{
		projectRoot: "/project",
		path:        "src/main.go",
		embedding:   encodeEmbedding(1, 0),
		model:       "model-a",
	}})
	storeDirectory := t.TempDir()
	firstReader, err := OpenWithANNDirectory(t.Context(), path, storeDirectory)
	require.NoError(t, err)
	_, firstCatalog, err := firstReader.prepareStore(t.Context(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, firstReader.Close())

	leasedReader, err := OpenReadyProject("/project", storeDirectory)
	require.NoError(t, err)
	secondReader, err := OpenWithFilters(t.Context(), path, storeDirectory, ProjectFilters{IncludePaths: []string{"src"}})
	require.NoError(t, err)
	_, activeCatalog, err := secondReader.prepareStore(t.Context(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, secondReader.Close())

	result, err := pruneStoreGenerations(storeDirectory)
	require.NoError(t, err)
	require.Zero(t, result.RemovedGenerations)
	require.Equal(t, 2, result.RetainedGenerations)
	require.DirExists(t, firstCatalog.Directory)
	require.DirExists(t, activeCatalog.Directory)

	require.NoError(t, leasedReader.Close())
	result, err = pruneStoreGenerations(storeDirectory)
	require.NoError(t, err)
	require.Equal(t, 1, result.RemovedGenerations)
	require.Equal(t, 1, result.RetainedGenerations)
	require.NoDirExists(t, firstCatalog.Directory)
	require.DirExists(t, activeCatalog.Directory)
}

func TestPruneStoreKeepsNewestCheckpointWithoutActiveCatalog(t *testing.T) {
	storeDirectory := t.TempDir()
	older := writePruneCheckpoint(t, storeDirectory, "generation-old", "/project", time.Now().Add(-2*time.Hour))
	newer := writePruneCheckpoint(t, storeDirectory, "generation-new", "/project", time.Now().Add(-time.Hour))

	result, err := pruneStoreGenerations(storeDirectory)
	require.NoError(t, err)
	require.Equal(t, 1, result.RemovedGenerations)
	require.Equal(t, 1, result.RetainedGenerations)
	require.NoDirExists(t, older)
	require.DirExists(t, newer)
}

func TestPruneStoreSkipsContendedProject(t *testing.T) {
	storeDirectory := t.TempDir()
	first := writePruneCheckpoint(t, storeDirectory, "generation-first", "/project", time.Now().Add(-2*time.Hour))
	second := writePruneCheckpoint(t, storeDirectory, "generation-second", "/project", time.Now().Add(-time.Hour))
	release, err := lock.TryFile(projectCatalogPath(storeDirectory, "/project") + ".lock")
	require.NoError(t, err)
	defer release()

	result, err := pruneStoreGenerations(storeDirectory)
	require.NoError(t, err)
	require.Zero(t, result.RemovedGenerations)
	require.Equal(t, 1, result.SkippedProjects)
	require.DirExists(t, first)
	require.DirExists(t, second)
}
