package codebaseindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	aikitann "github.com/townsendmerino/aikit/ann"
)

func TestStandaloneStorePartitionsByFolderAndSize(t *testing.T) {
	originalSegmentSize := storeBuildSegmentSize
	storeBuildSegmentSize = 2
	t.Cleanup(func() { storeBuildSegmentSize = originalSegmentSize })

	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "README.md", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "src/a.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "src/b.go", embedding: encodeEmbedding(0, 1), model: "model-a"},
		{projectRoot: "/project", path: "src/c.go", embedding: encodeEmbedding(-1, 0), model: "model-a"},
		{projectRoot: "/project", path: "test/a.go", embedding: encodeEmbedding(0, -1), model: "model-a"},
	})
	storeDirectory := t.TempDir()
	reader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	store, catalog, err := reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.True(t, catalog.Complete)
	require.Equal(t, 5, catalog.Chunks)
	require.Equal(t, 2, catalog.Dimension)
	require.Equal(t, []string{"", "src/", "src/", "test/"}, segmentPrefixes(catalog.Segments))
	require.Equal(t, 5, store.catalog.Chunks)
	require.FileExists(t, projectCatalogPath(storeDirectory, "/project"))
	for _, segment := range catalog.Segments {
		require.NoError(t, validateStoreSegment(catalog.Directory, segment))
		require.LessOrEqual(t, segment.Nodes, 2)
		index, err := aikitann.LoadHNSWMmap(filepath.Join(catalog.Directory, segment.Name, "index.hnsw"))
		require.NoError(t, err)
		require.Equal(t, segment.Nodes, index.Len())
		require.NoError(t, index.Close())
	}
}

func TestStandaloneStoreRepairsMalformedSmallHNSWSegment(t *testing.T) {
	first := make([]float32, 1024)
	first[0] = 1
	second := make([]float32, 1024)
	second[1] = 1
	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "README.md", chunkIndex: 0, embedding: encodeEmbedding(first...), model: "model-a"},
		{projectRoot: "/project", path: "README.md", chunkIndex: 1, embedding: encodeEmbedding(second...), model: "model-a"},
		{projectRoot: "/project", path: "src/main.go", embedding: encodeEmbedding(first...), model: "model-a"},
	})
	storeDirectory := t.TempDir()
	reader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	store, catalog, err := reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.Len(t, catalog.Segments, 2)
	require.Equal(t, "", catalog.Segments[0].Prefix)
	require.Equal(t, 2, catalog.Segments[0].Nodes)

	indexPath := filepath.Join(catalog.Directory, catalog.Segments[0].Name, "index.hnsw")
	index, err := aikitann.LoadHNSWMmap(indexPath)
	require.NoError(t, err)
	require.Equal(t, 2, index.Len())
	require.NoError(t, index.Close())

	malformed := aikitann.BuildHNSW([][]float32{first, second}, aikitann.Config{Int8: true})
	file, err := os.Create(indexPath)
	require.NoError(t, err)
	_, err = malformed.WriteTo(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	_, err = aikitann.LoadHNSWMmap(indexPath)
	require.ErrorContains(t, err, "HNSW blob count")

	scoped, err := store.search(context.Background(), first, SearchOptions{Limit: 1, MinScore: -1, PathPrefix: "src/"})
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	_, err = aikitann.LoadHNSWMmap(indexPath)
	require.ErrorContains(t, err, "HNSW blob count")

	unscoped, err := store.search(context.Background(), first, SearchOptions{Limit: 3, MinScore: -1})
	require.NoError(t, err)
	require.Len(t, unscoped, 3)
	index, err = aikitann.LoadHNSWMmap(indexPath)
	require.NoError(t, err)
	require.Equal(t, 2, index.Len())
	require.NoError(t, index.Close())
}

func TestStandaloneStoreSearchesWithoutSQLite(t *testing.T) {
	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "src/exact.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "test/other.go", embedding: encodeEmbedding(0, 1), model: "model-a"},
	})
	storeDirectory := t.TempDir()
	reader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	_, _, err = reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.NoError(t, os.Remove(path))

	standalone, err := OpenProject(context.Background(), "/project", filepath.Join(t.TempDir(), "missing"), storeDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, standalone.Close()) })
	require.Nil(t, standalone.db)

	results, err := standalone.Search(context.Background(), &fakeQueryEmbedder{embedding: []float32{1, 0}}, "/project", "exact", SearchOptions{Limit: 2, MinScore: -1})
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Equal(t, "src/exact.go", results[0].Chunk.Path)
	require.Equal(t, "content for src/exact.go", results[0].Chunk.Content)
	require.InDelta(t, 1, results[0].Score, 0.00001)
}

func TestStandaloneStoreFansOutAndMergesANNSegments(t *testing.T) {
	originalSegmentSize := storeBuildSegmentSize
	storeBuildSegmentSize = 1
	t.Cleanup(func() { storeBuildSegmentSize = originalSegmentSize })

	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "a/first.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "a/second.go", embedding: encodeEmbedding(0.8, 0.2), model: "model-a"},
		{projectRoot: "/project", path: "b/third.go", embedding: encodeEmbedding(0, 1), model: "model-a"},
		{projectRoot: "/project", path: "c/fourth.go", embedding: encodeEmbedding(-1, 0), model: "model-a"},
	})
	reader, err := OpenWithANNDirectory(context.Background(), path, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	store, catalog, err := reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.Len(t, catalog.Segments, 4)

	selected := store.selectedSegments("")
	require.Len(t, selected, 4)
	candidates, err := store.searchCandidates(context.Background(), []float32{1, 0}, selected, 2)
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	require.NotEqual(t, candidates[0].segment, candidates[1].segment)

	results, err := store.loadResults(context.Background(), []float32{1, 0}, candidates, SearchOptions{Limit: 2, MinScore: -1})
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Equal(t, "a/first.go", results[0].Chunk.Path)
	require.Equal(t, "a/second.go", results[1].Chunk.Path)
}

func TestStandaloneStoreRoutesPathPrefix(t *testing.T) {
	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "src/a.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "test/a.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
	})
	reader, err := OpenWithANNDirectory(context.Background(), path, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	store, _, err := reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)

	selected := store.selectedSegments("src/")
	require.Len(t, selected, 1)
	require.Equal(t, "src/", store.catalog.Segments[selected[0]].Prefix)
	results, err := store.search(context.Background(), []float32{1, 0}, SearchOptions{Limit: 10, MinScore: -1, PathPrefix: "src/"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "src/a.go", results[0].Chunk.Path)
}

func TestStandaloneStoreRoutesNestedPathPrefix(t *testing.T) {
	originalSegmentSize := storeBuildSegmentSize
	storeBuildSegmentSize = 1
	t.Cleanup(func() { storeBuildSegmentSize = originalSegmentSize })

	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "output/kemono/a.json", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "output/pixiv/a.json", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "output/pixiv/b.json", embedding: encodeEmbedding(0, 1), model: "model-a"},
	})
	reader, err := OpenWithANNDirectory(context.Background(), path, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	store, _, err := reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)

	selected := store.selectedSegments("output/pixiv/")
	require.Len(t, selected, 2)
	for _, segmentNumber := range selected {
		require.Equal(t, "output/", store.catalog.Segments[segmentNumber].Prefix)
		require.Contains(t, store.catalog.Segments[segmentNumber].FirstPath, "output/pixiv/")
	}
}

func TestStoreBuildPathRangeNormalizesEveryPath(t *testing.T) {
	chunks := []storeBuildChunk{
		{chunk: Chunk{Path: "/src/z.go"}},
		{chunk: Chunk{Path: "./src/a.go"}},
		{chunk: Chunk{Path: `src\middle.go`}},
	}

	firstPath, lastPath := storeBuildPathRange(chunks)
	require.Equal(t, "src/a.go", firstPath)
	require.Equal(t, "src/z.go", lastPath)
}

func TestStandaloneStoreUpgradesSegmentPathRanges(t *testing.T) {
	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "src/a.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "src/b.go", embedding: encodeEmbedding(0, 1), model: "model-a"},
	})
	storeDirectory := t.TempDir()
	reader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	_, catalog, err := reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	for index := range catalog.Segments {
		catalog.Segments[index].FirstPath = ""
		catalog.Segments[index].LastPath = ""
	}
	catalogPath := projectCatalogPath(storeDirectory, "/project")
	require.NoError(t, writeJSONAtomically(catalogPath, catalog))

	upgraded, err := loadProjectCatalog(storeDirectory, "/project")
	require.NoError(t, err)
	require.Equal(t, "src/a.go", upgraded.Segments[0].FirstPath)
	require.Equal(t, "src/b.go", upgraded.Segments[0].LastPath)
	reloaded, err := loadProjectCatalog(storeDirectory, "/project")
	require.NoError(t, err)
	require.Equal(t, upgraded.Segments, reloaded.Segments)
}

func TestStandaloneMigrationResumesCompletedSegments(t *testing.T) {
	originalSegmentSize := storeBuildSegmentSize
	storeBuildSegmentSize = 1
	t.Cleanup(func() { storeBuildSegmentSize = originalSegmentSize })

	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "src/a.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "src/b.go", embedding: encodeEmbedding(0, 1), model: "model-a"},
		{projectRoot: "/project", path: "src/c.go", embedding: encodeEmbedding(-1, 0), model: "model-a"},
	})
	storeDirectory := t.TempDir()
	reader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	_, catalog, err := reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.Len(t, catalog.Segments, 3)
	firstInfo, err := os.Stat(filepath.Join(catalog.Directory, catalog.Segments[0].Name, "index.hnsw"))
	require.NoError(t, err)

	firstRecordFile, err := os.Open(filepath.Join(catalog.Directory, catalog.Segments[0].Name, "records.bin"))
	require.NoError(t, err)
	firstRecord, err := readStoreRecord(firstRecordFile, 0)
	require.NoError(t, err)
	require.NoError(t, firstRecordFile.Close())
	firstData, err := os.Open(filepath.Join(catalog.Directory, catalog.Segments[0].Name, "data.bin"))
	require.NoError(t, err)
	pathBytes := make([]byte, firstRecord.PathLength)
	_, err = firstData.ReadAt(pathBytes, firstRecord.PathOffset)
	require.NoError(t, err)
	require.NoError(t, firstData.Close())

	for _, segment := range catalog.Segments[1:] {
		require.NoError(t, os.RemoveAll(filepath.Join(catalog.Directory, segment.Name)))
	}
	catalog.Complete = false
	catalog.Segments = catalog.Segments[:1]
	catalog.Chunks = 1
	catalog.LastPath = string(pathBytes)
	catalog.LastChunkIndex = firstRecord.ChunkIndex
	catalog.LastChunkID = firstRecord.ID
	require.NoError(t, writeJSONAtomically(filepath.Join(catalog.Directory, "migration.json"), catalog))
	require.NoError(t, os.Remove(projectCatalogPath(storeDirectory, "/project")))
	require.NoError(t, reader.Close())

	resumedReader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resumedReader.Close()) })
	_, resumed, err := resumedReader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.True(t, resumed.Complete)
	require.Equal(t, 3, resumed.Chunks)
	require.Len(t, resumed.Segments, 3)
	firstInfoAfter, err := os.Stat(filepath.Join(resumed.Directory, resumed.Segments[0].Name, "index.hnsw"))
	require.NoError(t, err)
	require.Equal(t, firstInfo.ModTime(), firstInfoAfter.ModTime())
}

func TestStandaloneMigrationPersistsPendingProgressOnFailure(t *testing.T) {
	originalRecordInterval := storeCheckpointRecordInterval
	originalTimeInterval := storeCheckpointTimeInterval
	storeCheckpointRecordInterval = 1000
	storeCheckpointTimeInterval = time.Hour
	t.Cleanup(func() {
		storeCheckpointRecordInterval = originalRecordInterval
		storeCheckpointTimeInterval = originalTimeInterval
	})

	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "src/a.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "src/b.go", embedding: []byte{1}, model: "model-a"},
	})
	storeDirectory := t.TempDir()
	reader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	_, _, err = reader.prepareStore(context.Background(), "/project", "model-a")
	require.ErrorContains(t, err, "decode embedding")

	checkpoints, err := filepath.Glob(filepath.Join(storeDirectory, "generation-*", "migration.json"))
	require.NoError(t, err)
	require.Len(t, checkpoints, 1)
	catalog, err := loadStoreCatalog(checkpoints[0])
	require.NoError(t, err)
	require.False(t, catalog.Complete)
	require.Equal(t, 1, catalog.FilesProcessed)
	require.Equal(t, 1, catalog.Chunks)
	require.Equal(t, "src/a.go", catalog.LastPath)
	require.Equal(t, "src/a.go", catalog.CurrentPath)
	require.Len(t, catalog.Segments, 1)
	require.NoError(t, validateStoreSegment(catalog.Directory, catalog.Segments[0]))
}

func TestStandaloneMigrationRecoversActivatedSegmentBeforeCheckpoint(t *testing.T) {
	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "src/a.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "src/b.go", embedding: encodeEmbedding(0, 1), model: "model-a"},
	})
	storeDirectory := t.TempDir()
	reader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	_, catalog, err := reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	segmentPath := filepath.Join(catalog.Directory, catalog.Segments[0].Name, "index.hnsw")
	segmentInfo, err := os.Stat(segmentPath)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(catalog.Directory, "migration.json")))
	require.NoError(t, os.Remove(projectCatalogPath(storeDirectory, "/project")))
	require.NoError(t, reader.Close())

	resumedReader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resumedReader.Close()) })
	_, resumed, err := resumedReader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.True(t, resumed.Complete)
	require.Equal(t, 2, resumed.Chunks)
	segmentInfoAfter, err := os.Stat(segmentPath)
	require.NoError(t, err)
	require.Equal(t, segmentInfo.ModTime(), segmentInfoAfter.ModTime())
}

func TestStandaloneStoreDetectsCorruptSegment(t *testing.T) {
	path := createTestDatabase(t, []testChunk{{projectRoot: "/project", path: "src/a.go", embedding: encodeEmbedding(1, 0), model: "model-a"}})
	storeDirectory := t.TempDir()
	reader, err := OpenWithANNDirectory(context.Background(), path, storeDirectory)
	require.NoError(t, err)
	_, catalog, err := reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.NoError(t, os.Truncate(filepath.Join(catalog.Directory, catalog.Segments[0].Name, "records.bin"), 1))

	_, err = loadProjectCatalog(storeDirectory, "/project")
	require.ErrorContains(t, err, "invalid records.bin size")
}

func TestTopLevelPathPrefix(t *testing.T) {
	require.Equal(t, "src/", topLevelPathPrefix("src/internal/file.go"))
	require.Equal(t, "src/", topLevelPathPrefix(`src\\internal\\file.go`))
	require.Empty(t, topLevelPathPrefix("README.md"))
	require.True(t, pathPrefixesOverlap("src/", "src/internal/"))
	require.False(t, pathPrefixesOverlap("src/", "test/"))
}

func TestStandaloneStoreAppliesProjectFilters(t *testing.T) {
	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "src/main.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "src/generated/output.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "test/main_test.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
	})
	storeDirectory := t.TempDir()
	filters := ProjectFilters{
		IncludePaths: []string{" ./src ", `src\`},
		ExcludePaths: []string{"src/generated"},
	}
	reader, err := OpenWithFilters(context.Background(), path, storeDirectory, filters)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	store, catalog, err := reader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.Equal(t, 1, catalog.Chunks)
	require.NotEmpty(t, catalog.Source.FilterDigest)
	results, err := store.search(context.Background(), []float32{1, 0}, SearchOptions{Limit: 10, MinScore: -1})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "src/main.go", results[0].Chunk.Path)
}

func TestStandaloneStoreFilterChangeUsesDistinctGeneration(t *testing.T) {
	path := createTestDatabase(t, []testChunk{
		{projectRoot: "/project", path: "src/main.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
		{projectRoot: "/project", path: "test/main_test.go", embedding: encodeEmbedding(1, 0), model: "model-a"},
	})
	storeDirectory := t.TempDir()

	sourceReader, err := OpenWithFilters(context.Background(), path, storeDirectory, ProjectFilters{IncludePaths: []string{"src"}})
	require.NoError(t, err)
	_, sourceCatalog, err := sourceReader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.NoError(t, sourceReader.Close())

	testReader, err := OpenWithFilters(context.Background(), path, storeDirectory, ProjectFilters{IncludePaths: []string{"test"}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testReader.Close()) })
	_, testCatalog, err := testReader.prepareStore(context.Background(), "/project", "model-a")
	require.NoError(t, err)
	require.NotEqual(t, sourceCatalog.Source.FilterDigest, testCatalog.Source.FilterDigest)
	require.NotEqual(t, sourceCatalog.Directory, testCatalog.Directory)
	require.DirExists(t, sourceCatalog.Directory)
	require.DirExists(t, testCatalog.Directory)
}

func TestNormalizeProjectFilters(t *testing.T) {
	filters := NormalizeProjectFilters(ProjectFilters{
		IncludePaths: []string{" ./src ", `src\`, "README.md", ""},
		ExcludePaths: []string{"/src/generated", "src/generated"},
	})
	require.Equal(t, []string{"README.md", "src"}, filters.IncludePaths)
	require.Equal(t, []string{"src/generated"}, filters.ExcludePaths)
	require.True(t, projectPathIncluded("src/main.go", filters))
	require.True(t, projectPathIncluded("README.md", filters))
	require.False(t, projectPathIncluded("src/generated/output.go", filters))
	require.False(t, projectPathIncluded("src2/main.go", filters))
}

func segmentPrefixes(segments []storeSegmentManifest) []string {
	prefixes := make([]string, len(segments))
	for index, segment := range segments {
		prefixes[index] = segment.Prefix
	}
	return prefixes
}
