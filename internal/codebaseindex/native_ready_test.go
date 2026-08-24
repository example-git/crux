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

type nativeReadyEmbedder struct {
	paths    []string
	failPath string
}

func (*nativeReadyEmbedder) PreferredEmbeddingModel(context.Context) string {
	return "model-a"
}

func (e *nativeReadyEmbedder) ChunkAndEmbedFile(_ context.Context, path, content, model string) ([]EmbeddedDocumentChunk, error) {
	e.paths = append(e.paths, path)
	if path == e.failPath {
		return nil, errors.New("embedding interrupted")
	}
	return []EmbeddedDocumentChunk{{
		Hash:      path,
		Text:      content,
		StartLine: 1,
		EndLine:   1,
		Model:     model,
		Embedding: []float32{1, 0},
	}}, nil
}

func TestNativeProjectIndexReportsProgress(t *testing.T) {
	projectRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "a.go"), []byte("package a\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "b.go"), []byte("package b\n"), 0o600))
	storeDirectory := t.TempDir()
	var progress []IndexProgress
	err := buildNativeProjectStore(
		t.Context(),
		projectRoot,
		storeDirectory,
		ProjectFilters{},
		&nativeReadyEmbedder{},
		func(update IndexProgress) { progress = append(progress, update) },
	)
	require.NoError(t, err)
	require.NotEmpty(t, progress)
	last := progress[len(progress)-1]
	require.Equal(t, "Complete", last.Stage)
	require.Equal(t, 2, last.FilesTotal)
	require.Equal(t, 2, last.FilesProcessed)
	require.Equal(t, 2, last.ChunksCreated)
	require.Zero(t, last.FilesSkipped)
	require.Empty(t, last.CurrentPath)

	status := InspectProjectIndexStatus(ProjectIndexOptions{
		ProjectRoot:    projectRoot,
		StoreDirectory: storeDirectory,
		Enabled:        true,
	})
	require.Equal(t, StoreStateReady, status.State)
	require.Equal(t, 2, status.FilesTotal)
	require.Equal(t, 2, status.FilesProcessed)
	require.Equal(t, 2, status.ChunksCreated)
	require.False(t, status.FinishedAt.IsZero())
}

func TestNativeProjectIndexReusesUnchangedFiles(t *testing.T) {
	projectRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "a.go"), []byte("package a\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "b.go"), []byte("package b\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "d.go"), []byte("package d\n"), 0o600))
	storeDirectory := t.TempDir()
	embedder := &nativeReadyEmbedder{}

	require.NoError(t, buildNativeProjectStore(t.Context(), projectRoot, storeDirectory, ProjectFilters{}, embedder, nil))
	require.Equal(t, []string{"a.go", "b.go", "d.go"}, embedder.paths)

	embedder.paths = nil
	require.NoError(t, os.Remove(filepath.Join(projectRoot, "a.go")))
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "b.go"), []byte("package b_changed\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "c.go"), []byte("package c\n"), 0o600))
	require.NoError(t, buildNativeProjectStore(t.Context(), projectRoot, storeDirectory, ProjectFilters{}, embedder, nil))
	require.Equal(t, []string{"b.go", "c.go"}, embedder.paths)

	reader, err := OpenReadyProject(projectRoot, storeDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	results, err := reader.Search(t.Context(), &fakeQueryEmbedder{embedding: []float32{1, 0}}, projectRoot, "package", SearchOptions{Limit: 10, MinScore: -1})
	require.NoError(t, err)
	paths := make([]string, 0, len(results))
	for _, result := range results {
		paths = append(paths, result.Chunk.Path)
	}
	require.ElementsMatch(t, []string{"b.go", "c.go", "d.go"}, paths)
}

func TestNativeProjectIndexResumesDurableFileProgressAfterFailure(t *testing.T) {
	originalFileInterval := nativeCheckpointFileInterval
	originalTimeInterval := nativeCheckpointTimeInterval
	nativeCheckpointFileInterval = 1
	nativeCheckpointTimeInterval = time.Hour
	t.Cleanup(func() {
		nativeCheckpointFileInterval = originalFileInterval
		nativeCheckpointTimeInterval = originalTimeInterval
	})

	projectRoot := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		require.NoError(t, os.WriteFile(filepath.Join(projectRoot, name), []byte("package test\n"), 0o600))
	}
	storeDirectory := t.TempDir()
	first := &nativeReadyEmbedder{failPath: "b.go"}
	err := buildNativeProjectStore(t.Context(), projectRoot, storeDirectory, ProjectFilters{}, first, nil)
	require.ErrorContains(t, err, "embedding interrupted")
	require.Equal(t, []string{"a.go", "b.go"}, first.paths)

	status := InspectProjectIndexStatus(ProjectIndexOptions{ProjectRoot: projectRoot, StoreDirectory: storeDirectory, Enabled: true})
	require.Equal(t, StoreStateMissing, status.State)
	require.Equal(t, "Indexing files", status.Stage)
	require.Equal(t, 3, status.FilesTotal)
	require.Equal(t, 1, status.FilesProcessed)
	require.Equal(t, 1, status.ChunksCreated)
	require.Equal(t, "b.go", status.CurrentPath)
	require.False(t, status.StartedAt.IsZero())

	second := &nativeReadyEmbedder{}
	require.NoError(t, buildNativeProjectStore(t.Context(), projectRoot, storeDirectory, ProjectFilters{}, second, nil))
	require.Equal(t, []string{"b.go", "c.go"}, second.paths)

	status = InspectProjectIndexStatus(ProjectIndexOptions{ProjectRoot: projectRoot, StoreDirectory: storeDirectory, Enabled: true})
	require.Equal(t, StoreStateReady, status.State)
	require.Equal(t, 3, status.FilesProcessed)
	require.Equal(t, 3, status.ChunksCreated)
}

func TestNativeProjectIndexReusesDurableProgressAfterSourceChange(t *testing.T) {
	originalFileInterval := nativeCheckpointFileInterval
	originalTimeInterval := nativeCheckpointTimeInterval
	nativeCheckpointFileInterval = 1
	nativeCheckpointTimeInterval = time.Hour
	t.Cleanup(func() {
		nativeCheckpointFileInterval = originalFileInterval
		nativeCheckpointTimeInterval = originalTimeInterval
	})

	projectRoot := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		require.NoError(t, os.WriteFile(filepath.Join(projectRoot, name), []byte("package test\n"), 0o600))
	}
	storeDirectory := t.TempDir()
	first := &nativeReadyEmbedder{failPath: "b.go"}
	err := buildNativeProjectStore(t.Context(), projectRoot, storeDirectory, ProjectFilters{}, first, nil)
	require.ErrorContains(t, err, "embedding interrupted")
	require.Equal(t, []string{"a.go", "b.go"}, first.paths)

	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "d.go"), []byte("package test\n"), 0o600))
	second := &nativeReadyEmbedder{}
	require.NoError(t, buildNativeProjectStore(t.Context(), projectRoot, storeDirectory, ProjectFilters{}, second, nil))
	require.Equal(t, []string{"b.go", "c.go", "d.go"}, second.paths)

	status := InspectProjectIndexStatus(ProjectIndexOptions{ProjectRoot: projectRoot, StoreDirectory: storeDirectory, Enabled: true})
	require.Equal(t, StoreStateReady, status.State)
	require.Equal(t, 4, status.FilesProcessed)
	require.Equal(t, 4, status.ChunksCreated)
}

func TestNativeProjectIndexPersistsSkippedOnlyProgress(t *testing.T) {
	projectRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "a.go"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, "b.go"), []byte("package b\n"), 0o600))
	storeDirectory := t.TempDir()

	first := &nativeReadyEmbedder{failPath: "b.go"}
	err := buildNativeProjectStore(t.Context(), projectRoot, storeDirectory, ProjectFilters{}, first, nil)
	require.ErrorContains(t, err, "embedding interrupted")
	status := InspectProjectIndexStatus(ProjectIndexOptions{ProjectRoot: projectRoot, StoreDirectory: storeDirectory, Enabled: true})
	require.Equal(t, 1, status.FilesProcessed)
	require.Equal(t, 1, status.FilesSkipped)
	require.Zero(t, status.ChunksCreated)

	second := &nativeReadyEmbedder{}
	require.NoError(t, buildNativeProjectStore(t.Context(), projectRoot, storeDirectory, ProjectFilters{}, second, nil))
	require.Equal(t, []string{"b.go"}, second.paths)
}

func TestOpenReadyNativeProjectDoesNotRequireFreshnessScan(t *testing.T) {
	projectRoot := t.TempDir()
	projectFile := filepath.Join(projectRoot, "main.go")
	require.NoError(t, os.WriteFile(projectFile, []byte("package main\n"), 0o600))
	storeDirectory := t.TempDir()

	require.NoError(t, buildNativeProjectStore(t.Context(), projectRoot, storeDirectory, ProjectFilters{}, &nativeReadyEmbedder{}, nil))
	require.NoError(t, os.Remove(projectFile))
	require.Equal(t, StoreStateStale, InspectProjectIndexStatus(ProjectIndexOptions{
		ProjectRoot:    projectRoot,
		StoreDirectory: storeDirectory,
		Enabled:        true,
	}).State)

	reader, err := OpenReadyProject(projectRoot, storeDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	require.Nil(t, reader.db)
	require.NotNil(t, reader.catalog)
	results, err := reader.Search(t.Context(), &fakeQueryEmbedder{embedding: []float32{1, 0}}, projectRoot, "main", SearchOptions{Limit: 1, MinScore: -1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "main.go", results[0].Chunk.Path)
}
