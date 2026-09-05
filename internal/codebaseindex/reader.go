package codebaseindex

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strings"

	cruxdb "github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/lock"
)

type Chunk struct {
	ID             int64
	ProjectRoot    string
	Path           string
	ChunkIndex     int
	Content        string
	Embedding      []float32
	EmbeddingNorm  float64
	EmbeddingModel string
	ChunkHash      string
	StartLine      int
	EndLine        int
	ContentHash    string
	Mtime          int64
}

type Dataset struct {
	Model     string
	Dimension int
	Chunks    []Chunk
}

type Reader struct {
	db           *sql.DB
	path         string
	annDirectory string
	catalog      *storeCatalog
	filters      ProjectFilters
	filterDigest string
	releaseLease func()
}

func Open(ctx context.Context, path string) (*Reader, error) {
	return OpenWithANNDirectory(ctx, path, "")
}

func OpenWithANNDirectory(ctx context.Context, path, annDirectory string) (*Reader, error) {
	return OpenWithFilters(ctx, path, annDirectory, ProjectFilters{})
}

func OpenWithFilters(ctx context.Context, path, storeDirectory string, filters ProjectFilters) (*Reader, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("codebase index database path is empty")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat codebase index database %q: %w", path, err)
	}

	database, err := cruxdb.ConnectReadOnly(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open codebase index database %q: %w", path, err)
	}
	storeDirectory, err = resolveStoreDirectory(storeDirectory)
	if err != nil {
		database.Close()
		return nil, err
	}
	filters = NormalizeProjectFilters(filters)
	return &Reader{
		db:           database,
		path:         path,
		annDirectory: storeDirectory,
		filters:      filters,
		filterDigest: filterDigest(filters),
	}, nil
}

func (r *Reader) Close() error {
	var err error
	if r.db != nil {
		err = r.db.Close()
		r.db = nil
	}
	if r.releaseLease != nil {
		r.releaseLease()
		r.releaseLease = nil
	}
	return err
}

func acquireGenerationLease(ctx context.Context, generationDirectory string) (func(), error) {
	return lock.SharedFile(ctx, generationLeasePath(generationDirectory))
}

func (r *Reader) Load(ctx context.Context, projectRoot, model string) (Dataset, error) {
	if r.db == nil {
		return Dataset{}, fmt.Errorf("source SQLite database is unavailable after standalone migration")
	}
	if strings.TrimSpace(projectRoot) == "" {
		return Dataset{}, fmt.Errorf("project root is empty")
	}
	if strings.TrimSpace(model) == "" {
		return Dataset{}, fmt.Errorf("embedding model is empty")
	}

	rows, err := r.db.QueryContext(ctx, `
SELECT id, project_root, path, chunk_index, content,
       embedding, embedding_norm, embedding_model,
       chunk_hash, start_line, end_line, content_hash, mtime
FROM chunks
WHERE project_root = ? AND embedding_model = ?
ORDER BY path, chunk_index`, projectRoot, model)
	if err != nil {
		return Dataset{}, fmt.Errorf("query codebase index chunks: %w", err)
	}
	defer rows.Close()

	dataset := Dataset{Model: model}
	for rows.Next() {
		var (
			chunk         Chunk
			embeddingBlob []byte
			chunkHash     sql.NullString
		)
		if err := rows.Scan(
			&chunk.ID,
			&chunk.ProjectRoot,
			&chunk.Path,
			&chunk.ChunkIndex,
			&chunk.Content,
			&embeddingBlob,
			&chunk.EmbeddingNorm,
			&chunk.EmbeddingModel,
			&chunkHash,
			&chunk.StartLine,
			&chunk.EndLine,
			&chunk.ContentHash,
			&chunk.Mtime,
		); err != nil {
			return Dataset{}, fmt.Errorf("scan codebase index chunk: %w", err)
		}

		embedding, err := decodeEmbedding(embeddingBlob)
		if err != nil {
			return Dataset{}, fmt.Errorf("decode embedding for chunk %d: %w", chunk.ID, err)
		}
		if dataset.Dimension == 0 {
			dataset.Dimension = len(embedding)
		} else if len(embedding) != dataset.Dimension {
			return Dataset{}, fmt.Errorf("chunk %d embedding dimension %d does not match model %q dimension %d", chunk.ID, len(embedding), model, dataset.Dimension)
		}

		chunk.Embedding = embedding
		chunk.ChunkHash = chunkHash.String
		dataset.Chunks = append(dataset.Chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return Dataset{}, fmt.Errorf("iterate codebase index chunks: %w", err)
	}
	return dataset, nil
}

func decodeEmbedding(blob []byte) ([]float32, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("embedding is empty")
	}
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("embedding byte length %d is not divisible by 4", len(blob))
	}

	embedding := make([]float32, len(blob)/4)
	for index := range embedding {
		bits := binary.LittleEndian.Uint32(blob[index*4 : index*4+4])
		value := math.Float32frombits(bits)
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("embedding contains a non-finite value at dimension %d", index)
		}
		embedding[index] = value
	}
	return embedding, nil
}
