package codebaseindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/example-git/crux/internal/semanticembedding"
)

type QueryEmbedder = semanticembedding.QueryEmbedder

type SearchOptions struct {
	Limit      int
	MinScore   float64
	PathPrefix string
}

type SearchResult struct {
	Chunk         Chunk
	Score         float64
	Symbol        string
	Role          SearchRole
	FacetCoverage float64
	Explanation   string
}

func betterSearchResult(left, right SearchResult) bool {
	if left.Score != right.Score {
		return left.Score > right.Score
	}
	if left.Chunk.Path != right.Chunk.Path {
		return left.Chunk.Path < right.Chunk.Path
	}
	return left.Chunk.ChunkIndex < right.Chunk.ChunkIndex
}

func (r *Reader) Model(ctx context.Context, projectRoot string) (string, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return "", fmt.Errorf("project root is empty")
	}
	if r.catalog != nil && r.catalog.Complete && r.catalog.ProjectRoot == projectRoot {
		return r.catalog.Model, nil
	}
	if r.db == nil {
		if catalog, err := loadProjectCatalog(r.annDirectory, projectRoot); err == nil {
			r.catalog = &catalog
			return catalog.Model, nil
		}
		return "", fmt.Errorf("no standalone codebase search store found for project %q", projectRoot)
	}

	var model string
	err := r.db.QueryRowContext(ctx, `
SELECT embedding_model
FROM index_metadata
WHERE project_root = ? AND stale = 0
LIMIT 1`, projectRoot).Scan(&model)
	if err == nil && strings.TrimSpace(model) != "" {
		return model, nil
	}

	err = r.db.QueryRowContext(ctx, `
SELECT embedding_model
FROM chunks
WHERE project_root = ?
GROUP BY embedding_model
ORDER BY COUNT(*) DESC, embedding_model
LIMIT 1`, projectRoot).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no codebase index found for project %q", projectRoot)
	}
	if err != nil {
		return "", fmt.Errorf("find embedding model for project %q: %w", projectRoot, err)
	}
	return model, nil
}

func (r *Reader) Search(ctx context.Context, embedder QueryEmbedder, projectRoot, query string, options SearchOptions) ([]SearchResult, error) {
	if embedder == nil {
		return nil, fmt.Errorf("query embedder is nil")
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("search query is empty")
	}
	if options.Limit == 0 {
		options.Limit = 10
	}
	if options.Limit < 0 {
		return nil, fmt.Errorf("search result limit must be positive")
	}

	model, err := r.Model(ctx, projectRoot)
	if err != nil {
		return nil, err
	}
	queryEmbedding, err := embedder.EmbedQuery(ctx, query, model)
	if err != nil {
		return nil, fmt.Errorf("embed search query: %w", err)
	}
	if len(queryEmbedding) == 0 {
		return nil, fmt.Errorf("query embedding is empty")
	}
	if !finiteEmbedding(queryEmbedding) {
		return nil, fmt.Errorf("query embedding contains a non-finite value")
	}
	queryNorm := vectorNorm(queryEmbedding)
	if queryNorm == 0 {
		return nil, fmt.Errorf("query embedding has zero norm")
	}
	for dimension := range queryEmbedding {
		queryEmbedding[dimension] /= float32(queryNorm)
	}

	store, catalog, err := r.prepareStore(ctx, projectRoot, model)
	if err != nil {
		return nil, fmt.Errorf("prepare standalone codebase search store: %w", err)
	}
	if len(queryEmbedding) != catalog.Dimension {
		return nil, fmt.Errorf("query embedding dimension %d does not match model %q dimension %d", len(queryEmbedding), model, catalog.Dimension)
	}
	return store.search(ctx, queryEmbedding, options)
}

func vectorNorm(vector []float32) float64 {
	var sum float64
	for _, value := range vector {
		sum += float64(value) * float64(value)
	}
	return math.Sqrt(sum)
}

func dotProduct(left, right []float32) float64 {
	var sum float64
	for index := range left {
		sum += float64(left[index]) * float64(right[index])
	}
	return sum
}
