// Package semanticembedding defines provider-neutral embedding contracts used
// by codebase indexing and retrieval.
package semanticembedding

import (
	"context"
	"math"
)

type EmbeddedDocumentChunk struct {
	Hash      string
	Text      string
	StartLine int
	EndLine   int
	Model     string
	Embedding []float32
}

type ModelSelector interface {
	PreferredEmbeddingModel(context.Context) string
}

type DocumentEmbedder interface {
	ModelSelector
	ChunkAndEmbedFile(context.Context, string, string, string) ([]EmbeddedDocumentChunk, error)
}

type QueryEmbedder interface {
	EmbedQuery(context.Context, string, string) ([]float32, error)
}

type Embedder interface {
	DocumentEmbedder
	QueryEmbedder
}

func Finite(vector []float32) bool {
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}
