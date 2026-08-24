package codebaseindex

import (
	"context"
	"database/sql"
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type testChunk struct {
	projectRoot   string
	path          string
	chunkIndex    int
	embedding     []byte
	embeddingNorm float64
	model         string
	chunkHash     any
}

func TestReaderLoad(t *testing.T) {
	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "b.go", chunkIndex: 1, embedding: encodeEmbedding(3, 4), model: "model-a", chunkHash: nil},
		{projectRoot: "/project", path: "a.go", chunkIndex: 0, embedding: encodeEmbedding(1, 2), model: "model-a", chunkHash: "chunk-a"},
		{projectRoot: "/project", path: "ignored.go", chunkIndex: 0, embedding: encodeEmbedding(9, 9), model: "model-b", chunkHash: "ignored"},
		{projectRoot: "/other", path: "ignored.go", chunkIndex: 0, embedding: encodeEmbedding(8, 8), model: "model-a", chunkHash: "ignored"},
	})

	reader, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	dataset, err := reader.Load(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.Equal(t, "model-a", dataset.Model)
	require.Equal(t, 2, dataset.Dimension)
	require.Len(t, dataset.Chunks, 2)
	require.Equal(t, "a.go", dataset.Chunks[0].Path)
	require.Equal(t, []float32{1, 2}, dataset.Chunks[0].Embedding)
	require.Equal(t, "chunk-a", dataset.Chunks[0].ChunkHash)
	require.Equal(t, "b.go", dataset.Chunks[1].Path)
	require.Empty(t, dataset.Chunks[1].ChunkHash)
}

func TestReaderLoadNoMatches(t *testing.T) {
	path := createTestDatabase(t, nil)
	reader, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	dataset, err := reader.Load(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.Equal(t, Dataset{Model: "model-a"}, dataset)
}

func TestReaderLoadRejectsInvalidEmbedding(t *testing.T) {
	t.Run("malformed byte length", func(t *testing.T) {
		path := createTestDatabase(t, []testChunk{{projectRoot: "/project", path: "a.go", embedding: []byte{1, 2, 3}, model: "model-a"}})
		reader, err := Open(context.Background(), path)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })

		_, err = reader.Load(context.Background(), "/project", "model-a")
		require.ErrorContains(t, err, "is not divisible by 4")
	})

	t.Run("dimension mismatch", func(t *testing.T) {
		path := createTestDatabase(t, []testChunk{
			{projectRoot: "/project", path: "a.go", embedding: encodeEmbedding(1, 2), model: "model-a"},
			{projectRoot: "/project", path: "b.go", embedding: encodeEmbedding(1, 2, 3), model: "model-a"},
		})
		reader, err := Open(context.Background(), path)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })

		_, err = reader.Load(context.Background(), "/project", "model-a")
		require.ErrorContains(t, err, "does not match")
	})

	t.Run("non-finite value", func(t *testing.T) {
		path := createTestDatabase(t, []testChunk{{projectRoot: "/project", path: "a.go", embedding: encodeEmbedding(float32(math.Inf(1))), model: "model-a"}})
		reader, err := Open(context.Background(), path)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })

		_, err = reader.Load(context.Background(), "/project", "model-a")
		require.ErrorContains(t, err, "non-finite")
	})
}

func TestReaderRejectsInvalidInputs(t *testing.T) {
	_, err := Open(context.Background(), "")
	require.ErrorContains(t, err, "path is empty")

	_, err = Open(context.Background(), filepath.Join(t.TempDir(), "missing.db"))
	require.ErrorContains(t, err, "stat codebase index database")

	path := createTestDatabase(t, nil)
	reader, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	_, err = reader.Load(context.Background(), "", "model-a")
	require.ErrorContains(t, err, "project root is empty")
	_, err = reader.Load(context.Background(), "/project", "")
	require.ErrorContains(t, err, "embedding model is empty")
}

func createTestDatabase(t *testing.T, chunks []testChunk) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codebase-index.db")
	database, err := sql.Open("sqlite", path)
	require.NoError(t, err)

	_, err = database.ExecContext(t.Context(), `
CREATE TABLE chunks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_root TEXT NOT NULL,
    path TEXT NOT NULL,
    chunk_index INTEGER NOT NULL,
    content TEXT NOT NULL,
    embedding BLOB NOT NULL,
    embedding_norm REAL NOT NULL DEFAULT 0,
    embedding_model TEXT NOT NULL,
    chunk_hash TEXT,
    start_line INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0,
    content_hash TEXT NOT NULL,
    mtime INTEGER NOT NULL
)`)
	require.NoError(t, err)

	for _, chunk := range chunks {
		embeddingNorm := chunk.embeddingNorm
		if embeddingNorm == 0 && len(chunk.embedding)%4 == 0 {
			values, decodeErr := decodeEmbedding(chunk.embedding)
			if decodeErr == nil {
				embeddingNorm = vectorNorm(values)
			}
		}
		_, err = database.ExecContext(t.Context(), `
INSERT INTO chunks (
    project_root, path, chunk_index, content, embedding, embedding_norm,
    embedding_model, chunk_hash, start_line, end_line, content_hash, mtime
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			chunk.projectRoot,
			chunk.path,
			chunk.chunkIndex,
			"content for "+chunk.path,
			chunk.embedding,
			embeddingNorm,
			chunk.model,
			chunk.chunkHash,
			10,
			20,
			"content-hash",
			1234,
		)
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())
	return path
}

func encodeEmbedding(values ...float32) []byte {
	blob := make([]byte, len(values)*4)
	for index, value := range values {
		binary.LittleEndian.PutUint32(blob[index*4:index*4+4], math.Float32bits(value))
	}
	return blob
}
