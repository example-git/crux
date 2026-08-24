package codebaseindex

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSearchIntegration(t *testing.T) {
	databasePath := os.Getenv("CODEBASE_INDEX_INTEGRATION_DB")
	projectRoot := os.Getenv("CODEBASE_INDEX_INTEGRATION_ROOT")
	if databasePath == "" || projectRoot == "" {
		t.Skip("CODEBASE_INDEX_INTEGRATION_DB and CODEBASE_INDEX_INTEGRATION_ROOT are required")
	}

	annDirectory := os.Getenv("CODEBASE_INDEX_INTEGRATION_ANN_DIR")
	if annDirectory == "" {
		annDirectory = t.TempDir()
	}
	pathPrefix := os.Getenv("CODEBASE_INDEX_INTEGRATION_PATH_PREFIX")
	reader, err := OpenWithANNDirectory(context.Background(), databasePath, annDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	ctx := context.Background()
	started := time.Now()
	model, err := reader.Model(ctx, projectRoot)
	require.NoError(t, err)
	t.Logf("model lookup: %s", time.Since(started))

	client := NewGitHubClient(http.DefaultClient, CodebaseIndexToken, "crux-codebase-search-integration")
	started = time.Now()
	query, err := client.EmbedQuery(ctx, "download and process Pixiv artwork", model)
	require.NoError(t, err)
	t.Logf("query embedding: %s", time.Since(started))
	queryNorm := vectorNorm(query)
	for dimension := range query {
		query[dimension] /= float32(queryNorm)
	}

	started = time.Now()
	store, catalog, err := reader.prepareStore(ctx, projectRoot, model)
	require.NoError(t, err)
	require.Equal(t, catalog.Dimension, len(query))
	t.Logf("standalone store preparation: %s", time.Since(started))

	started = time.Now()
	selected := store.selectedSegments(pathPrefix)
	candidates, err := store.searchCandidates(ctx, query, selected, 100)
	require.NoError(t, err)
	require.NotEmpty(t, candidates)
	t.Logf("mmap segment search (%d/%d segments): %s", len(selected), len(catalog.Segments), time.Since(started))

	started = time.Now()
	results, err := store.loadResults(ctx, query, candidates, SearchOptions{Limit: 10, MinScore: -1, PathPrefix: pathPrefix})
	require.NoError(t, err)
	require.NotEmpty(t, results)
	t.Logf("standalone candidate load: %s", time.Since(started))
}
