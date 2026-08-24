package codebaseindex

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

const (
	recallGateMean  = 0.98
	recallGateWorst = 0.90
	recallGateTop1  = 0.99
)

type recallQuery struct {
	vector  []float32
	options SearchOptions
}

type recallFixture struct {
	reader  *Reader
	chunks  []storeBuildChunk
	queries []recallQuery
}

type recallEmbedder struct {
	vector []float32
}

func (e recallEmbedder) EmbedQuery(context.Context, string, string) ([]float32, error) {
	return append([]float32(nil), e.vector...), nil
}

type recallMetrics struct {
	mean  float64
	worst float64
	top1  float64
}

var recallBenchmarkResults []SearchResult

func TestStandaloneStoreEndToEndRecall(t *testing.T) {
	fixture := newRecallFixture(t)
	metrics := measureRecall(t, fixture)
	requireRecallThresholds(t, metrics)
}

func TestStoreSearchEF(t *testing.T) {
	tests := []struct {
		name       string
		candidates int
		nodes      int
		want       int
	}{
		{name: "minimum breadth", candidates: 10, nodes: 5000, want: 256},
		{name: "candidate multiple", candidates: 200, nodes: 5000, want: 400},
		{name: "segment bound", candidates: 200, nodes: 300, want: 300},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := storeSearchEF(test.candidates, test.nodes); got != test.want {
				t.Fatalf("storeSearchEF(%d, %d) = %d, want %d", test.candidates, test.nodes, got, test.want)
			}
		})
	}
}

func BenchmarkStandaloneStoreEndToEndRecall(b *testing.B) {
	fixture := newRecallFixture(b)
	metrics := measureRecall(b, fixture)
	requireRecallThresholds(b, metrics)
	b.ReportAllocs()
	b.ResetTimer()

	queryIndex := 0
	for b.Loop() {
		query := fixture.queries[queryIndex%len(fixture.queries)]
		results, err := fixture.reader.Search(context.Background(), recallEmbedder{vector: query.vector}, "/project", "recall query", query.options)
		if err != nil {
			b.Fatal(err)
		}
		recallBenchmarkResults = results
		queryIndex++
	}
	b.ReportMetric(metrics.mean*100, "mean_recall@10")
	b.ReportMetric(metrics.worst*100, "worst_recall@10")
	b.ReportMetric(metrics.top1*100, "top1_agreement")
}

func newRecallFixture(tb testing.TB) recallFixture {
	tb.Helper()
	const (
		segmentCount    = 4
		nodesPerSegment = 640
		dimension       = 48
		clusterCount    = 16
	)

	rng := rand.New(rand.NewSource(7))
	centers := make([][]float32, clusterCount)
	for index := range centers {
		centers[index] = randomUnitVector(rng, dimension)
	}

	directory := tb.TempDir()
	catalog := storeCatalog{
		Version:     storeCatalogVersion,
		Complete:    true,
		ProjectRoot: "/project",
		Model:       "recall-model",
		Dimension:   dimension,
		Directory:   directory,
	}
	allChunks := make([]storeBuildChunk, 0, segmentCount*nodesPerSegment)
	for segmentNumber := range segmentCount {
		prefix := "src/"
		if segmentNumber >= segmentCount/2 {
			prefix = "test/"
		}
		chunks := make([]storeBuildChunk, 0, nodesPerSegment)
		for node := range nodesPerSegment {
			center := centers[(segmentNumber*nodesPerSegment+node)%len(centers)]
			vector := make([]float32, dimension)
			for dimensionIndex := range vector {
				vector[dimensionIndex] = center[dimensionIndex] + float32(rng.NormFloat64()*0.08)
			}
			normalizeTestVector(vector)
			id := int64(segmentNumber*nodesPerSegment + node + 1)
			path := fmt.Sprintf("%ssegment-%02d/file-%04d.go", prefix, segmentNumber, node)
			value := storeBuildChunk{
				chunk: Chunk{
					ID:         id,
					Path:       path,
					ChunkIndex: node,
					Content:    "content for " + path,
					StartLine:  node + 1,
					EndLine:    node + 2,
				},
				vector: vector,
			}
			chunks = append(chunks, value)
			allChunks = append(allChunks, value)
		}
		manifest, err := saveStoreSegment(directory, segmentNumber, prefix, dimension, chunks)
		if err != nil {
			tb.Fatal(err)
		}
		catalog.Segments = append(catalog.Segments, manifest)
		catalog.Chunks += len(chunks)
	}

	queries := make([]recallQuery, 0, 30)
	for index := range 30 {
		chunk := allChunks[(index*83+29)%len(allChunks)]
		vector := append([]float32(nil), chunk.vector...)
		options := SearchOptions{Limit: 10, MinScore: -1}
		switch index % 3 {
		case 1:
			options.PathPrefix = topLevelPathPrefix(chunk.chunk.Path)
		case 2:
			options.PathPrefix = topLevelPathPrefix(chunk.chunk.Path)
			options.MinScore = 0.5
		}
		queries = append(queries, recallQuery{vector: vector, options: options})
	}
	return recallFixture{reader: &Reader{catalog: &catalog}, chunks: allChunks, queries: queries}
}

func randomUnitVector(rng *rand.Rand, dimension int) []float32 {
	vector := make([]float32, dimension)
	for index := range vector {
		vector[index] = float32(rng.NormFloat64())
	}
	normalizeTestVector(vector)
	return vector
}

func normalizeTestVector(vector []float32) {
	norm := vectorNorm(vector)
	for index := range vector {
		vector[index] /= float32(norm)
	}
}

func measureRecall(tb testing.TB, fixture recallFixture) recallMetrics {
	tb.Helper()
	metrics := recallMetrics{worst: 1}
	for _, query := range fixture.queries {
		truth := exactRecallResults(fixture.chunks, query.vector, query.options)
		if len(truth) != query.options.Limit {
			tb.Fatalf("exact oracle returned %d results, want %d", len(truth), query.options.Limit)
		}
		results, err := fixture.reader.Search(context.Background(), recallEmbedder{vector: query.vector}, "/project", "recall query", query.options)
		if err != nil {
			tb.Fatal(err)
		}
		truthIDs := make(map[int64]struct{}, len(truth))
		for _, result := range truth {
			truthIDs[result.Chunk.ID] = struct{}{}
		}
		hits := 0
		for _, result := range results {
			if _, exists := truthIDs[result.Chunk.ID]; exists {
				hits++
			}
		}
		recall := float64(hits) / float64(len(truth))
		metrics.mean += recall
		metrics.worst = min(metrics.worst, recall)
		if len(results) > 0 && results[0].Chunk.ID == truth[0].Chunk.ID {
			metrics.top1++
		}
	}
	metrics.mean /= float64(len(fixture.queries))
	metrics.top1 /= float64(len(fixture.queries))
	return metrics
}

func exactRecallResults(chunks []storeBuildChunk, query []float32, options SearchOptions) []SearchResult {
	pathPrefix := normalizeIndexPath(options.PathPrefix)
	results := make([]SearchResult, 0, len(chunks))
	for _, value := range chunks {
		if pathPrefix != "" && !strings.HasPrefix(normalizeIndexPath(value.chunk.Path), pathPrefix) {
			continue
		}
		score := dotProduct(query, value.vector)
		if score < options.MinScore {
			continue
		}
		results = append(results, SearchResult{Chunk: value.chunk, Score: score})
	}
	sortSearchResults(results)
	if len(results) > options.Limit {
		results = results[:options.Limit]
	}
	return results
}

func requireRecallThresholds(tb testing.TB, metrics recallMetrics) {
	tb.Helper()
	if metrics.mean < recallGateMean {
		tb.Errorf("mean recall@10 %.4f is below %.2f", metrics.mean, recallGateMean)
	}
	if metrics.worst < recallGateWorst {
		tb.Errorf("worst-query recall@10 %.4f is below %.2f", metrics.worst, recallGateWorst)
	}
	if metrics.top1 < recallGateTop1 {
		tb.Errorf("top-1 agreement %.4f is below %.2f", metrics.top1, recallGateTop1)
	}
}
