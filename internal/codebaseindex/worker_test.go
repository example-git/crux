package codebaseindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenReadyProjectReportsUnavailable(t *testing.T) {
	reader, err := OpenReadyProject("/project", t.TempDir())
	require.Nil(t, reader)
	var unavailable *StoreUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, StoreStateMissing, unavailable.State)
}

func TestStartProjectIndexingIsNonblockingAndDeduplicated(t *testing.T) {
	resetBackgroundIndexes(t)
	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	runProjectIndexing = func(_ context.Context, _ string, _ string, _ string, _ ProjectFilters, report func(IndexProgress)) error {
		calls++
		report(IndexProgress{
			Stage:          "Indexing files",
			FilesTotal:     10,
			FilesProcessed: 3,
			ChunksCreated:  24,
			FilesSkipped:   1,
			CurrentPath:    "src/main.go",
		})
		close(started)
		<-release
		return nil
	}

	storeDirectory := t.TempDir()
	status := StartProjectIndexing(context.Background(), "/project", "/database", storeDirectory)
	require.Equal(t, StoreStateIndexing, status.State)
	require.False(t, status.Serving)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background indexing did not start")
	}

	duplicate := StartProjectIndexing(context.Background(), "/project", "/database", storeDirectory)
	require.Equal(t, StoreStateIndexing, duplicate.State)
	require.Equal(t, 10, duplicate.FilesTotal)
	require.Equal(t, 3, duplicate.FilesProcessed)
	require.Equal(t, 24, duplicate.ChunksCreated)
	require.Equal(t, 1, duplicate.FilesSkipped)
	require.Equal(t, "src/main.go", duplicate.CurrentPath)
	require.Equal(t, 1, calls)
	reader, err := OpenReadyProject("/project", storeDirectory)
	require.Nil(t, reader)
	var unavailable *StoreUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, StoreStateIndexing, unavailable.State)
	close(release)
	require.Eventually(t, func() bool {
		backgroundIndexes.RLock()
		defer backgroundIndexes.RUnlock()
		return backgroundIndexes.jobs[projectIndexKey("/project", storeDirectory)].status.State == StoreStateReady
	}, time.Second, 10*time.Millisecond)
}

func TestOpenReadyProjectServesActiveGenerationDuringRefresh(t *testing.T) {
	resetBackgroundIndexes(t)
	path := createTestDatabase(t, []testChunk{{
		projectRoot: "/project",
		path:        "src/main.go",
		embedding:   encodeEmbedding(1, 0),
		model:       "model-a",
	}})
	storeDirectory := t.TempDir()
	sourceReader, err := OpenWithANNDirectory(t.Context(), path, storeDirectory)
	require.NoError(t, err)
	_, _, err = sourceReader.prepareStore(t.Context(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, sourceReader.Close())
	future := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(path, future, future))

	started := make(chan struct{})
	release := make(chan struct{})
	runProjectIndexing = func(context.Context, string, string, string, ProjectFilters, func(IndexProgress)) error {
		close(started)
		<-release
		return nil
	}
	status := StartProjectIndexing(t.Context(), "/project", path, storeDirectory)
	require.Equal(t, StoreStateIndexing, status.State)
	require.True(t, status.Serving)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not start")
	}

	reader, err := OpenReadyProject("/project", storeDirectory)
	require.NoError(t, err)
	results, err := reader.Search(t.Context(), &fakeQueryEmbedder{embedding: []float32{1, 0}}, "/project", "main", SearchOptions{Limit: 1, MinScore: -1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "src/main.go", results[0].Chunk.Path)
	require.NoError(t, reader.Close())
	close(release)
	require.Eventually(t, func() bool {
		backgroundIndexes.RLock()
		defer backgroundIndexes.RUnlock()
		return backgroundIndexes.jobs[projectIndexKey("/project", storeDirectory)].status.State == StoreStateReady
	}, time.Second, 10*time.Millisecond)
}

func TestReconcileRestoresDurableProgressAfterProcessRestart(t *testing.T) {
	resetBackgroundIndexes(t)
	projectRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "a.go"), []byte("package a\n"), 0o600))
	storeDirectory := t.TempDir()
	_, source, err := nativeProjectSource(t.Context(), projectRoot, ProjectFilters{})
	require.NoError(t, err)
	generationDirectory := storeGenerationDirectory(storeDirectory, projectRoot, "model-a", source)
	require.NoError(t, os.MkdirAll(generationDirectory, 0o700))
	startedAt := time.Now().Add(-time.Minute).Round(0)
	require.NoError(t, writeJSONAtomically(filepath.Join(generationDirectory, "migration.json"), storeCatalog{
		Version:           storeCatalogVersion,
		ProjectRoot:       projectRoot,
		Model:             "model-a",
		Directory:         generationDirectory,
		Source:            source,
		FilesTotal:        1000,
		FilesProcessed:    640,
		FilesSkipped:      12,
		Chunks:            900,
		CurrentPath:       "src/current.go",
		Stage:             "Indexing files",
		StartedAt:         startedAt,
		ProgressUpdatedAt: time.Now(),
	}))

	release := make(chan struct{})
	runProjectIndexing = func(context.Context, string, string, string, ProjectFilters, func(IndexProgress)) error {
		<-release
		return errors.New("stop test worker")
	}
	status := ReconcileProjectIndexing(t.Context(), ProjectIndexOptions{ProjectRoot: projectRoot, StoreDirectory: storeDirectory, Enabled: true})
	require.Equal(t, StoreStateIndexing, status.State)
	require.Equal(t, "Resuming index", status.Stage)
	require.Equal(t, 1000, status.FilesTotal)
	require.Equal(t, 640, status.FilesProcessed)
	require.Equal(t, 900, status.ChunksCreated)
	require.Equal(t, 12, status.FilesSkipped)
	require.True(t, startedAt.Equal(status.StartedAt))
	close(release)
	require.Eventually(t, func() bool {
		return InspectProjectIndexStatus(ProjectIndexOptions{ProjectRoot: projectRoot, StoreDirectory: storeDirectory, Enabled: true}).State == StoreStateFailed
	}, time.Second, 10*time.Millisecond)
}

func TestCompleteStatusCountsSkippedFilesAsProcessed(t *testing.T) {
	status := statusWithCatalog(ProjectIndexOptions{ProjectRoot: "/project", Enabled: true}, t.TempDir(), storeCatalog{
		Complete:       true,
		ProjectRoot:    "/project",
		FilesTotal:     14942,
		FilesProcessed: 14224,
		FilesSkipped:   718,
		Source:         storeSource{Mode: "native"},
	}, StoreStatus{State: StoreStateReady})

	require.Equal(t, StoreStateReady, status.State)
	require.Equal(t, 14942, status.FilesProcessed)
	require.Equal(t, 718, status.FilesSkipped)
}

func TestStartProjectIndexingTransitionsToFailed(t *testing.T) {
	resetBackgroundIndexes(t)
	runProjectIndexing = func(context.Context, string, string, string, ProjectFilters, func(IndexProgress)) error {
		return errors.New("migration failed")
	}
	storeDirectory := t.TempDir()
	status := StartProjectIndexing(context.Background(), "/project", "/database", storeDirectory)
	require.Equal(t, StoreStateIndexing, status.State)
	require.Eventually(t, func() bool {
		return ProjectIndexStatus("/project", storeDirectory).State == StoreStateFailed
	}, time.Second, 10*time.Millisecond)

	_, err := OpenReadyProject("/project", storeDirectory)
	var unavailable *StoreUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, StoreStateFailed, unavailable.State)
	require.ErrorContains(t, unavailable, "migration failed")
}

func TestBackgroundIndexingProducesReadyStandaloneStore(t *testing.T) {
	resetBackgroundIndexes(t)
	path := createTestDatabase(t, []testChunk{{
		projectRoot: "/project",
		path:        "src/main.go",
		embedding:   encodeEmbedding(1, 0),
		model:       "model-a",
	}})
	storeDirectory := t.TempDir()
	status := StartProjectIndexing(context.Background(), "/project", path, storeDirectory)
	require.Equal(t, StoreStateIndexing, status.State)
	require.Eventually(t, func() bool {
		return ProjectIndexStatus("/project", storeDirectory).State == StoreStateReady
	}, 5*time.Second, 10*time.Millisecond)

	reader, err := OpenReadyProject("/project", storeDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	require.Nil(t, reader.db)
	results, err := reader.Search(context.Background(), &fakeQueryEmbedder{embedding: []float32{1, 0}}, "/project", "main", SearchOptions{Limit: 1, MinScore: -1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "src/main.go", results[0].Chunk.Path)
}

func TestOpenReadyProjectServesStaleSource(t *testing.T) {
	path := createTestDatabase(t, []testChunk{{
		projectRoot: "/project",
		path:        "src/main.go",
		embedding:   encodeEmbedding(1, 0),
		model:       "model-a",
	}})
	storeDirectory := t.TempDir()
	reader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	_, _, err = reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	future := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(path, future, future))
	status := ProjectIndexStatus("/project", storeDirectory)
	require.Equal(t, StoreStateStale, status.State)
	require.True(t, status.Serving)
	ready, err := OpenReadyProject("/project", storeDirectory)
	require.NoError(t, err)
	require.NoError(t, ready.Close())
}

func TestReconcileProjectIndexingDisablesAndCancelsWorker(t *testing.T) {
	resetBackgroundIndexes(t)
	started := make(chan struct{})
	canceled := make(chan struct{})
	runProjectIndexing = func(ctx context.Context, _ string, _ string, _ string, _ ProjectFilters, _ func(IndexProgress)) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	options := ProjectIndexOptions{
		ProjectRoot:    "/project",
		StoreDirectory: t.TempDir(),
		Enabled:        true,
	}
	require.Equal(t, StoreStateIndexing, ReconcileProjectIndexing(context.Background(), options).State)
	require.Eventually(t, func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	options.Enabled = false
	require.Equal(t, StoreStateDisabled, ReconcileProjectIndexing(context.Background(), options).State)
	require.Equal(t, StoreStateDisabled, InspectProjectIndexStatus(options).State)
	require.Eventually(t, func() bool {
		select {
		case <-canceled:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestReadyProjectRequiresMatchingFilters(t *testing.T) {
	resetBackgroundIndexes(t)
	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "src/main.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "test/main_test.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
	})
	storeDirectory := t.TempDir()
	sourceFilters := ProjectFilters{IncludePaths: []string{"src"}}
	reader, err := OpenWithFilters(context.Background(), path, storeDirectory, sourceFilters)
	require.NoError(t, err)
	_, _, err = reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, reader.Close())

	require.Equal(t, StoreStateReady, ProjectIndexStatusWithFilters("/project", storeDirectory, sourceFilters).State)
	testFilters := ProjectFilters{IncludePaths: []string{"test"}}
	require.Equal(t, StoreStateMissing, ProjectIndexStatusWithFilters("/project", storeDirectory, testFilters).State)
	ready, err := OpenReadyProjectWithFilters("/project", storeDirectory, testFilters)
	require.Nil(t, ready)
	var unavailable *StoreUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, StoreStateMissing, unavailable.State)
}

func resetBackgroundIndexes(t *testing.T) {
	t.Helper()
	originalRunner := runProjectIndexing
	backgroundIndexes.Lock()
	originalJobs := backgroundIndexes.jobs
	originalNextID := backgroundIndexes.nextID
	backgroundIndexes.jobs = make(map[string]backgroundIndexJob)
	backgroundIndexes.nextID = 0
	backgroundIndexes.Unlock()
	t.Cleanup(func() {
		runProjectIndexing = originalRunner
		backgroundIndexes.Lock()
		for _, job := range backgroundIndexes.jobs {
			if job.cancel != nil {
				job.cancel()
			}
		}
		backgroundIndexes.jobs = originalJobs
		backgroundIndexes.nextID = originalNextID
		backgroundIndexes.Unlock()
	})
}
