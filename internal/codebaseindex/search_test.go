package codebaseindex

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeQueryEmbedder struct {
	embedding []float32
	err       error
	query     string
	model     string
}

func (f *fakeQueryEmbedder) EmbedQuery(_ context.Context, query, model string) ([]float32, error) {
	f.query = query
	f.model = model
	return f.embedding, f.err
}

func TestReaderSearch(t *testing.T) {
	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "src/exact.go", chunkIndex: 0, embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "src/partial.go", chunkIndex: 0, embedding: encodeEmbedding(1, 1), model: "model-a"},
		{projectRoot: "/project", path: "test/opposite.go", chunkIndex: 0, embedding: encodeEmbedding(-1, 0), model: "model-a"},
		{projectRoot: "/project", path: "ignored.go", chunkIndex: 0, embedding: encodeEmbedding(1, 0), model: "model-b"},
	})
	reader, err := OpenWithANNDirectory(context.Background(), path, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	embedder := &fakeQueryEmbedder{embedding: []float32{1, 0}}
	results, err := reader.Search(context.Background(), embedder, "/project", "find exact", SearchOptions{
		Limit:      5,
		MinScore:   0.5,
		PathPrefix: "src/",
	})
	require.NoError(t, err)
	require.Equal(t, "find exact", embedder.query)
	require.Equal(t, "model-a", embedder.model)
	require.Len(t, results, 2)
	require.Equal(t, "src/exact.go", results[0].Chunk.Path)
	require.InDelta(t, 1, results[0].Score, 0.00001)
	require.Equal(t, "src/partial.go", results[1].Chunk.Path)
	require.InDelta(t, 0.707106, results[1].Score, 0.00001)

	results, err = reader.Search(context.Background(), embedder, "/project", "find exact", SearchOptions{Limit: 1, MinScore: -1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "src/exact.go", results[0].Chunk.Path)
}

func TestReaderSearchUsesDefaultLimit(t *testing.T) {
	chunks := make([]testChunk, 12)
	for index := range chunks {
		chunks[index] = testChunk{
			projectRoot: "/project",
			path:        string(rune('a'+index)) + ".go",
			chunkIndex:  index,
			embedding:   encodeEmbedding(1, 0),
			model:       "model-a",
		}
	}
	path := createTestDatabase(t, chunks)
	reader, err := OpenWithANNDirectory(context.Background(), path, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	results, err := reader.Search(context.Background(), &fakeQueryEmbedder{embedding: []float32{1, 0}}, "/project", "query", SearchOptions{})
	require.NoError(t, err)
	require.Len(t, results, 10)
	require.Equal(t, "a.go", results[0].Chunk.Path)
	require.Equal(t, "j.go", results[9].Chunk.Path)
}

func TestReaderSearchRejectsInvalidInputs(t *testing.T) {
	path := createTestDatabase(t, []testChunk{{projectRoot: "/project", path: "a.go", embedding: encodeEmbedding(1, 0), model: "model-a"}})
	reader, err := OpenWithANNDirectory(context.Background(), path, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	ctx := context.Background()

	_, err = reader.Search(ctx, nil, "/project", "query", SearchOptions{})
	require.ErrorContains(t, err, "embedder is nil")
	_, err = reader.Search(ctx, &fakeQueryEmbedder{}, "/project", " ", SearchOptions{})
	require.ErrorContains(t, err, "query is empty")
	_, err = reader.Search(ctx, &fakeQueryEmbedder{}, "/project", "query", SearchOptions{Limit: -1})
	require.ErrorContains(t, err, "limit must be positive")
	_, err = reader.Search(ctx, &fakeQueryEmbedder{err: errors.New("unavailable")}, "/project", "query", SearchOptions{})
	require.ErrorContains(t, err, "embed search query: unavailable")
	_, err = reader.Search(ctx, &fakeQueryEmbedder{embedding: []float32{1}}, "/project", "query", SearchOptions{})
	require.ErrorContains(t, err, "dimension 1")
	_, err = reader.Search(ctx, &fakeQueryEmbedder{embedding: []float32{0, 0}}, "/project", "query", SearchOptions{})
	require.ErrorContains(t, err, "zero norm")
	_, err = reader.Search(ctx, &fakeQueryEmbedder{embedding: []float32{float32(math.NaN()), 0}}, "/project", "query", SearchOptions{})
	require.ErrorContains(t, err, "non-finite")
	_, err = reader.Search(ctx, &fakeQueryEmbedder{embedding: []float32{1, 0}}, "/missing", "query", SearchOptions{})
	require.ErrorContains(t, err, "no codebase index found")
}
