package codebaseindex

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/klauspost/compress/zstd"
	aikitann "github.com/townsendmerino/aikit/ann"
)

const (
	storeCatalogVersion   = 2
	storeRecordSize       = 64
	storeSegmentSize      = 5000
	storeSearchMinimumEF  = 256
	storeSearchEFMultiple = 2
)

var (
	storeBuildSegmentSize         = storeSegmentSize
	storeCheckpointRecordInterval = 1000
	storeCheckpointTimeInterval   = 5 * time.Second
)

type sourceFileState struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"mod_time"`
}

type storeSource struct {
	DatabasePath      string          `json:"database_path"`
	Database          sourceFileState `json:"database"`
	WAL               sourceFileState `json:"wal"`
	FilterDigest      string          `json:"filter_digest,omitempty"`
	Mode              string          `json:"mode"`
	MetadataUpdated   int64           `json:"metadata_updated,omitempty"`
	RunUpdated        int64           `json:"run_updated,omitempty"`
	ChunkCount        int64           `json:"chunk_count,omitempty"`
	MaxChunkID        int64           `json:"max_chunk_id,omitempty"`
	MaxMtime          int64           `json:"max_mtime,omitempty"`
	SumChunkID        int64           `json:"sum_chunk_id,omitempty"`
	NativeProjectRoot string          `json:"native_project_root,omitempty"`
	NativeFingerprint string          `json:"native_fingerprint,omitempty"`
	NativeFilters     string          `json:"native_filters,omitempty"`
}

type storeSegmentManifest struct {
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	FirstPath string `json:"first_path,omitempty"`
	LastPath  string `json:"last_path,omitempty"`
	Nodes     int    `json:"nodes"`
	Dimension int    `json:"dimension"`
}

type storeCatalog struct {
	Version           int                    `json:"version"`
	Complete          bool                   `json:"complete"`
	ProjectRoot       string                 `json:"project_root"`
	Model             string                 `json:"model"`
	Dimension         int                    `json:"dimension"`
	Chunks            int                    `json:"chunks"`
	FilesTotal        int                    `json:"files_total,omitempty"`
	FilesProcessed    int                    `json:"files_processed,omitempty"`
	FilesSkipped      int                    `json:"files_skipped,omitempty"`
	CurrentPath       string                 `json:"current_path,omitempty"`
	Stage             string                 `json:"stage,omitempty"`
	StartedAt         time.Time              `json:"started_at,omitempty"`
	ProgressUpdatedAt time.Time              `json:"progress_updated_at,omitempty"`
	NativeFiles       []nativeProjectFile    `json:"native_files,omitempty"`
	NativeSkipped     []string               `json:"native_skipped,omitempty"`
	IndexedAt         time.Time              `json:"indexed_at,omitempty"`
	Directory         string                 `json:"directory"`
	Segments          []storeSegmentManifest `json:"segments"`
	Source            storeSource            `json:"source"`
	LastPath          string                 `json:"last_path,omitempty"`
	LastChunkIndex    int                    `json:"last_chunk_index,omitempty"`
	LastChunkID       int64                  `json:"last_chunk_id,omitempty"`
	LastCompletedPath string                 `json:"last_completed_path,omitempty"`
}

type storeBuildChunk struct {
	chunk  Chunk
	vector []float32
}

type storeRecord struct {
	ID               int64
	ChunkIndex       int
	StartLine        int
	EndLine          int
	PathOffset       int64
	PathLength       int
	ContentOffset    int64
	ContentLength    int
	RawContentLength int
}

type storeCandidate struct {
	segment int
	index   int
	score   float64
}

type storeCandidateHeap []storeCandidate

func (h storeCandidateHeap) Len() int           { return len(h) }
func (h storeCandidateHeap) Less(i, j int) bool { return h[i].score < h[j].score }
func (h storeCandidateHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *storeCandidateHeap) Push(value any)    { *h = append(*h, value.(storeCandidate)) }
func (h *storeCandidateHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

type segmentStore struct {
	catalog storeCatalog
}

var storeBuildLocks = struct {
	sync.Mutex
	values map[string]*sync.Mutex
}{values: make(map[string]*sync.Mutex)}

func DefaultStoreDirectory() (string, error) {
	base := os.Getenv("AI_CLI_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".ai-cli")
	}
	return filepath.Join(base, "codebase-index-store"), nil
}

func OpenProject(ctx context.Context, projectRoot, configuredDatabasePath, storeDirectory string) (*Reader, error) {
	return OpenProjectWithFilters(ctx, projectRoot, configuredDatabasePath, storeDirectory, ProjectFilters{})
}

func OpenProjectWithFilters(ctx context.Context, projectRoot, configuredDatabasePath, storeDirectory string, filters ProjectFilters) (*Reader, error) {
	directory, err := resolveStoreDirectory(storeDirectory)
	if err != nil {
		return nil, err
	}
	filters = NormalizeProjectFilters(filters)
	digest := filterDigest(filters)
	catalog, catalogErr := loadProjectCatalog(directory, projectRoot)
	if catalogErr == nil && catalog.Source.FilterDigest == digest && sourceFilesCurrent(catalog.Source) {
		return &Reader{
			path:         catalog.Source.DatabasePath,
			annDirectory: directory,
			catalog:      &catalog,
			filters:      filters,
			filterDigest: digest,
		}, nil
	}

	databasePath, err := ResolveDatabasePath(ctx, projectRoot, configuredDatabasePath)
	if err != nil {
		return nil, err
	}
	return OpenWithFilters(ctx, databasePath, directory, filters)
}

func resolveStoreDirectory(directory string) (string, error) {
	var err error
	if strings.TrimSpace(directory) == "" {
		directory, err = DefaultStoreDirectory()
		if err != nil {
			return "", err
		}
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve codebase search store directory %q: %w", directory, err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create codebase search store directory %q: %w", directory, err)
	}
	return directory, nil
}

func sourceFilesCurrent(source storeSource) bool {
	if source.Mode == "native" {
		return nativeSourceCurrent(source)
	}
	if source.DatabasePath == "" {
		return true
	}
	current, err := sourceFileStates(source.DatabasePath)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return err == nil && current.Database == source.Database && current.WAL == source.WAL
}

func sourceFileStates(databasePath string) (storeSource, error) {
	database, err := statFileState(databasePath)
	if err != nil {
		return storeSource{}, err
	}
	wal, err := statOptionalFileState(databasePath + "-wal")
	if err != nil {
		return storeSource{}, err
	}
	return storeSource{DatabasePath: databasePath, Database: database, WAL: wal}, nil
}

func statFileState(path string) (sourceFileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return sourceFileState{}, err
	}
	return sourceFileState{Size: info.Size(), ModTime: info.ModTime().UnixNano()}, nil
}

func statOptionalFileState(path string) (sourceFileState, error) {
	state, err := statFileState(path)
	if errors.Is(err, os.ErrNotExist) {
		return sourceFileState{}, nil
	}
	return state, err
}

func (r *Reader) prepareStore(ctx context.Context, projectRoot, model string) (*segmentStore, storeCatalog, error) {
	if r.catalog != nil && r.catalog.Complete && r.catalog.ProjectRoot == projectRoot && r.catalog.Model == model && r.catalog.Source.FilterDigest == r.filterDigest {
		return &segmentStore{catalog: *r.catalog}, *r.catalog, nil
	}
	if r.db == nil {
		return nil, storeCatalog{}, fmt.Errorf("standalone codebase search store is unavailable for project %q", projectRoot)
	}

	source, err := r.storeSource(ctx, projectRoot, model)
	if err != nil {
		return nil, storeCatalog{}, err
	}
	catalogPath := projectCatalogPath(r.annDirectory, projectRoot)
	if catalog, err := loadStoreCatalog(catalogPath); err == nil && catalog.matches(projectRoot, model, source) {
		r.catalog = &catalog
		return &segmentStore{catalog: catalog}, catalog, nil
	}

	buildLock := storeBuildLock(catalogPath)
	buildLock.Lock()
	defer buildLock.Unlock()
	if catalog, err := loadStoreCatalog(catalogPath); err == nil && catalog.matches(projectRoot, model, source) {
		r.catalog = &catalog
		return &segmentStore{catalog: catalog}, catalog, nil
	}

	release, err := lock.File(ctx, catalogPath+".lock")
	if err != nil {
		return nil, storeCatalog{}, fmt.Errorf("lock codebase search migration: %w", err)
	}
	defer release()
	if catalog, err := loadStoreCatalog(catalogPath); err == nil && catalog.matches(projectRoot, model, source) {
		r.catalog = &catalog
		return &segmentStore{catalog: catalog}, catalog, nil
	}

	catalog, err := r.migrateStore(ctx, catalogPath, projectRoot, model, source)
	if err != nil {
		return nil, storeCatalog{}, err
	}
	r.catalog = &catalog
	return &segmentStore{catalog: catalog}, catalog, nil
}

func storeBuildLock(path string) *sync.Mutex {
	storeBuildLocks.Lock()
	defer storeBuildLocks.Unlock()
	value := storeBuildLocks.values[path]
	if value == nil {
		value = &sync.Mutex{}
		storeBuildLocks.values[path] = value
	}
	return value
}

func (r *Reader) storeSource(ctx context.Context, projectRoot, model string) (storeSource, error) {
	source, err := sourceFileStates(r.path)
	if err != nil {
		return storeSource{}, fmt.Errorf("stat source codebase index: %w", err)
	}
	source.FilterDigest = r.filterDigest
	source.Mode = "checkpoint"
	err = r.db.QueryRowContext(ctx, `
SELECT
    COALESCE((SELECT updated_at FROM index_metadata WHERE project_root = ? AND embedding_model = ?), 0),
    COALESCE((SELECT updated_at FROM index_runs WHERE project_root = ?), 0)`,
		projectRoot,
		model,
		projectRoot,
	).Scan(&source.MetadataUpdated, &source.RunUpdated)
	if err == nil && source.MetadataUpdated > 0 {
		return source, nil
	}

	source.Mode = "aggregate"
	err = r.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(MAX(id), 0), COALESCE(MAX(mtime), 0), COALESCE(SUM(id), 0)
FROM chunks
WHERE project_root = ? AND embedding_model = ?`, projectRoot, model).Scan(
		&source.ChunkCount,
		&source.MaxChunkID,
		&source.MaxMtime,
		&source.SumChunkID,
	)
	if err != nil {
		return storeSource{}, fmt.Errorf("fingerprint source codebase index: %w", err)
	}
	if source.ChunkCount == 0 {
		return storeSource{}, fmt.Errorf("no embeddings found for project %q and model %q", projectRoot, model)
	}
	return source, nil
}

func (r *Reader) migrateStore(ctx context.Context, catalogPath, projectRoot, model string, source storeSource) (storeCatalog, error) {
	generationDirectory := storeGenerationDirectory(r.annDirectory, projectRoot, model, source)
	if err := os.MkdirAll(generationDirectory, 0o700); err != nil {
		return storeCatalog{}, fmt.Errorf("create codebase search generation: %w", err)
	}
	checkpointPath := filepath.Join(generationDirectory, "migration.json")
	catalog, err := loadStoreCatalog(checkpointPath)
	if err != nil || !catalog.partialMatches(projectRoot, model, source, generationDirectory) {
		catalog = storeCatalog{
			Version:           storeCatalogVersion,
			ProjectRoot:       projectRoot,
			Model:             model,
			Directory:         generationDirectory,
			Source:            source,
			FilesTotal:        int(source.ChunkCount),
			Stage:             "Importing database",
			StartedAt:         time.Now(),
			ProgressUpdatedAt: time.Now(),
		}
	}
	if catalog.StartedAt.IsZero() {
		catalog.StartedAt = time.Now()
	}
	catalog.Stage = "Importing database"
	catalog.ProgressUpdatedAt = time.Now()
	if err := writeJSONAtomically(checkpointPath, catalog); err != nil {
		return storeCatalog{}, fmt.Errorf("initialize standalone migration checkpoint: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
SELECT id, path, chunk_index, content, embedding, start_line, end_line
FROM chunks
WHERE project_root = ? AND embedding_model = ? AND (
    path > ? OR
    (path = ? AND chunk_index > ?) OR
    (path = ? AND chunk_index = ? AND id > ?)
)
ORDER BY path, chunk_index, id`,
		projectRoot,
		model,
		catalog.LastPath,
		catalog.LastPath,
		catalog.LastChunkIndex,
		catalog.LastPath,
		catalog.LastChunkIndex,
		catalog.LastChunkID,
	)
	if err != nil {
		return storeCatalog{}, fmt.Errorf("query chunks for standalone migration: %w", err)
	}
	defer rows.Close()

	chunks := make([]storeBuildChunk, 0, storeBuildSegmentSize)
	prefix := ""
	lastScanned := Chunk{ID: catalog.LastChunkID, Path: catalog.LastPath, ChunkIndex: catalog.LastChunkIndex}
	recordsSinceCheckpoint := 0
	lastCheckpointAt := time.Now()
	persist := func() error {
		catalog.LastPath = lastScanned.Path
		catalog.LastChunkIndex = lastScanned.ChunkIndex
		catalog.LastChunkID = lastScanned.ID
		catalog.CurrentPath = lastScanned.Path
		catalog.Stage = "Importing database"
		catalog.ProgressUpdatedAt = time.Now()
		if err := writeJSONAtomically(checkpointPath, catalog); err != nil {
			return fmt.Errorf("checkpoint standalone migration: %w", err)
		}
		recordsSinceCheckpoint = 0
		lastCheckpointAt = time.Now()
		return nil
	}
	flush := func() error {
		if len(chunks) > 0 {
			segment, err := saveStoreSegment(generationDirectory, len(catalog.Segments), prefix, catalog.Dimension, chunks)
			if err != nil {
				return err
			}
			catalog.Segments = append(catalog.Segments, segment)
			catalog.Chunks += len(chunks)
			chunks = make([]storeBuildChunk, 0, storeBuildSegmentSize)
		}
		return persist()
	}
	checkpointScanned := func(chunk Chunk) error {
		lastScanned = chunk
		catalog.FilesProcessed++
		recordsSinceCheckpoint++
		if recordsSinceCheckpoint >= storeCheckpointRecordInterval || time.Since(lastCheckpointAt) >= storeCheckpointTimeInterval {
			return flush()
		}
		return nil
	}
	stop := func(cause error) (storeCatalog, error) {
		if err := flush(); err != nil {
			return storeCatalog{}, err
		}
		return storeCatalog{}, cause
	}

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return stop(err)
		}
		var chunk Chunk
		var embeddingBlob []byte
		if err := rows.Scan(
			&chunk.ID,
			&chunk.Path,
			&chunk.ChunkIndex,
			&chunk.Content,
			&embeddingBlob,
			&chunk.StartLine,
			&chunk.EndLine,
		); err != nil {
			return stop(fmt.Errorf("scan chunk for standalone migration: %w", err))
		}
		if !projectPathIncluded(chunk.Path, r.filters) {
			if err := checkpointScanned(chunk); err != nil {
				return storeCatalog{}, err
			}
			continue
		}
		vector, err := decodeEmbedding(embeddingBlob)
		if err != nil {
			return stop(fmt.Errorf("decode embedding for chunk %d: %w", chunk.ID, err))
		}
		if catalog.Dimension == 0 {
			catalog.Dimension = len(vector)
		} else if len(vector) != catalog.Dimension {
			return stop(fmt.Errorf("chunk %d embedding dimension %d does not match model %q dimension %d", chunk.ID, len(vector), model, catalog.Dimension))
		}
		norm := vectorNorm(vector)
		if norm == 0 {
			if err := checkpointScanned(chunk); err != nil {
				return storeCatalog{}, err
			}
			continue
		}
		for dimension := range vector {
			vector[dimension] /= float32(norm)
		}
		chunkPrefix := topLevelPathPrefix(chunk.Path)
		if len(chunks) > 0 && (len(chunks) >= storeBuildSegmentSize || chunkPrefix != prefix) {
			if err := flush(); err != nil {
				return storeCatalog{}, err
			}
		}
		if len(chunks) == 0 {
			prefix = chunkPrefix
		}
		chunks = append(chunks, storeBuildChunk{chunk: chunk, vector: vector})
		if err := checkpointScanned(chunk); err != nil {
			return storeCatalog{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return stop(fmt.Errorf("iterate chunks for standalone migration: %w", err))
	}
	if err := flush(); err != nil {
		return storeCatalog{}, err
	}
	if catalog.Chunks == 0 {
		if len(r.filters.IncludePaths) > 0 || len(r.filters.ExcludePaths) > 0 {
			return storeCatalog{}, fmt.Errorf("project path filters excluded all non-zero embeddings for project %q and model %q", projectRoot, model)
		}
		return storeCatalog{}, fmt.Errorf("no non-zero embeddings found for project %q and model %q", projectRoot, model)
	}

	currentSource, err := r.storeSource(ctx, projectRoot, model)
	if err != nil {
		return storeCatalog{}, err
	}
	if currentSource != source {
		return storeCatalog{}, fmt.Errorf("source codebase index changed during standalone migration")
	}
	catalog.Complete = true
	catalog.CurrentPath = ""
	catalog.Stage = "Complete"
	catalog.ProgressUpdatedAt = time.Now()
	catalog.IndexedAt = time.Now()
	if err := writeJSONAtomically(checkpointPath, catalog); err != nil {
		return storeCatalog{}, fmt.Errorf("complete standalone migration checkpoint: %w", err)
	}
	if err := writeJSONAtomically(catalogPath, catalog); err != nil {
		return storeCatalog{}, fmt.Errorf("activate standalone codebase search catalog: %w", err)
	}
	return catalog, nil
}

func saveStoreSegment(directory string, segmentNumber int, prefix string, dimension int, chunks []storeBuildChunk) (storeSegmentManifest, error) {
	name := fmt.Sprintf("segment-%06d", segmentNumber)
	firstPath, lastPath := storeBuildPathRange(chunks)
	manifest := storeSegmentManifest{
		Name:      name,
		Prefix:    prefix,
		FirstPath: firstPath,
		LastPath:  lastPath,
		Nodes:     len(chunks),
		Dimension: dimension,
	}
	finalDirectory := filepath.Join(directory, name)
	if info, err := os.Stat(finalDirectory); err == nil && info.IsDir() {
		if err := validateRecoveredStoreSegment(directory, manifest, chunks); err != nil {
			return storeSegmentManifest{}, fmt.Errorf("recover standalone segment %q: %w", finalDirectory, err)
		}
		return manifest, nil
	}
	temporaryDirectory, err := os.MkdirTemp(directory, ".segment-*")
	if err != nil {
		return storeSegmentManifest{}, fmt.Errorf("create temporary standalone segment: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	if err := os.Chmod(temporaryDirectory, 0o700); err != nil {
		return storeSegmentManifest{}, err
	}

	vectorsPath := filepath.Join(temporaryDirectory, "vectors.f32")
	recordsPath := filepath.Join(temporaryDirectory, "records.bin")
	dataPath := filepath.Join(temporaryDirectory, "data.bin")
	indexPath := filepath.Join(temporaryDirectory, "index.hnsw")
	vectorsFile, err := createPrivateFile(vectorsPath)
	if err != nil {
		return storeSegmentManifest{}, err
	}
	recordsFile, err := createPrivateFile(recordsPath)
	if err != nil {
		vectorsFile.Close()
		return storeSegmentManifest{}, err
	}
	dataFile, err := createPrivateFile(dataPath)
	if err != nil {
		vectorsFile.Close()
		recordsFile.Close()
		return storeSegmentManifest{}, err
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	if err != nil {
		vectorsFile.Close()
		recordsFile.Close()
		dataFile.Close()
		return storeSegmentManifest{}, err
	}
	defer encoder.Close()

	vectors := make([][]float32, len(chunks))
	var dataOffset int64
	for index, value := range chunks {
		vectors[index] = value.vector
		if err := writeFloat32Vector(vectorsFile, value.vector); err != nil {
			return storeSegmentManifest{}, closeStoreBuildFiles(vectorsFile, recordsFile, dataFile, err)
		}
		pathBytes := []byte(value.chunk.Path)
		if _, err := dataFile.Write(pathBytes); err != nil {
			return storeSegmentManifest{}, closeStoreBuildFiles(vectorsFile, recordsFile, dataFile, err)
		}
		compressedContent := encoder.EncodeAll([]byte(value.chunk.Content), nil)
		contentOffset := dataOffset + int64(len(pathBytes))
		if _, err := dataFile.Write(compressedContent); err != nil {
			return storeSegmentManifest{}, closeStoreBuildFiles(vectorsFile, recordsFile, dataFile, err)
		}
		record := storeRecord{
			ID:               value.chunk.ID,
			ChunkIndex:       value.chunk.ChunkIndex,
			StartLine:        value.chunk.StartLine,
			EndLine:          value.chunk.EndLine,
			PathOffset:       dataOffset,
			PathLength:       len(pathBytes),
			ContentOffset:    contentOffset,
			ContentLength:    len(compressedContent),
			RawContentLength: len(value.chunk.Content),
		}
		if err := writeStoreRecord(recordsFile, record); err != nil {
			return storeSegmentManifest{}, closeStoreBuildFiles(vectorsFile, recordsFile, dataFile, err)
		}
		dataOffset = contentOffset + int64(len(compressedContent))
	}
	if err := syncAndCloseFiles(vectorsFile, recordsFile, dataFile); err != nil {
		return storeSegmentManifest{}, err
	}

	if err := writeStoreHNSW(indexPath, vectors); err != nil {
		return storeSegmentManifest{}, fmt.Errorf("save standalone vector index: %w", err)
	}
	if err := os.Rename(temporaryDirectory, finalDirectory); err != nil {
		return storeSegmentManifest{}, fmt.Errorf("activate standalone segment: %w", err)
	}
	return manifest, nil
}

func storeHNSWConfig(nodes int) aikitann.Config {
	return aikitann.Config{Int8: nodes >= 4}
}

func writeStoreHNSW(path string, vectors [][]float32) error {
	index := aikitann.BuildHNSW(vectors, storeHNSWConfig(len(vectors)))
	return writeFileAtomically(path, func(writer io.Writer) error {
		_, err := index.WriteTo(writer)
		return err
	})
}

func validateRecoveredStoreSegment(directory string, manifest storeSegmentManifest, chunks []storeBuildChunk) error {
	if err := validateStoreSegment(directory, manifest); err != nil {
		return err
	}
	segmentDirectory := filepath.Join(directory, manifest.Name)
	indexPath := filepath.Join(segmentDirectory, "index.hnsw")
	index, err := aikitann.LoadHNSWMmap(indexPath)
	if err == nil && index.Len() != len(chunks) {
		_ = index.Close()
		index = nil
		err = fmt.Errorf("vector index does not match expected shape")
	}
	if err != nil {
		vectors := make([][]float32, len(chunks))
		for position := range chunks {
			vectors[position] = chunks[position].vector
		}
		if err := writeStoreHNSW(indexPath, vectors); err != nil {
			return fmt.Errorf("rebuild vector index: %w", err)
		}
		index, err = aikitann.LoadHNSWMmap(indexPath)
		if err != nil {
			return fmt.Errorf("load rebuilt vector index: %w", err)
		}
	}
	if index.Len() != len(chunks) || len(index.Query(chunks[0].vector, 1)) != 1 {
		_ = index.Close()
		return fmt.Errorf("vector index does not match expected shape")
	}
	if err := index.Close(); err != nil {
		return fmt.Errorf("close vector index: %w", err)
	}

	recordsFile, err := os.Open(filepath.Join(segmentDirectory, "records.bin"))
	if err != nil {
		return err
	}
	defer recordsFile.Close()
	for position, chunk := range chunks {
		record, err := readStoreRecord(recordsFile, position)
		if err != nil {
			return err
		}
		if record.ID != chunk.chunk.ID || record.ChunkIndex != chunk.chunk.ChunkIndex {
			return fmt.Errorf("record %d does not match source chunk", position)
		}
	}
	return nil
}

func createPrivateFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func closeStoreBuildFiles(vectors, records, data *os.File, cause error) error {
	_ = vectors.Close()
	_ = records.Close()
	_ = data.Close()
	return cause
}

func syncAndCloseFiles(files ...*os.File) error {
	for _, file := range files {
		if err := file.Sync(); err != nil {
			for _, remaining := range files {
				_ = remaining.Close()
			}
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func writeFloat32Vector(writer io.Writer, vector []float32) error {
	buffer := make([]byte, len(vector)*4)
	for index, value := range vector {
		binary.LittleEndian.PutUint32(buffer[index*4:], math.Float32bits(value))
	}
	_, err := writer.Write(buffer)
	return err
}

func writeStoreRecord(writer io.Writer, record storeRecord) error {
	var buffer [storeRecordSize]byte
	binary.LittleEndian.PutUint64(buffer[0:], uint64(record.ID))
	binary.LittleEndian.PutUint32(buffer[8:], uint32(record.ChunkIndex))
	binary.LittleEndian.PutUint32(buffer[12:], uint32(record.StartLine))
	binary.LittleEndian.PutUint32(buffer[16:], uint32(record.EndLine))
	binary.LittleEndian.PutUint64(buffer[24:], uint64(record.PathOffset))
	binary.LittleEndian.PutUint32(buffer[32:], uint32(record.PathLength))
	binary.LittleEndian.PutUint64(buffer[40:], uint64(record.ContentOffset))
	binary.LittleEndian.PutUint32(buffer[48:], uint32(record.ContentLength))
	binary.LittleEndian.PutUint32(buffer[52:], uint32(record.RawContentLength))
	_, err := writer.Write(buffer[:])
	return err
}

func readStoreRecord(reader io.ReaderAt, index int) (storeRecord, error) {
	var buffer [storeRecordSize]byte
	if _, err := reader.ReadAt(buffer[:], int64(index*storeRecordSize)); err != nil {
		return storeRecord{}, err
	}
	return storeRecord{
		ID:               int64(binary.LittleEndian.Uint64(buffer[0:])),
		ChunkIndex:       int(int32(binary.LittleEndian.Uint32(buffer[8:]))),
		StartLine:        int(int32(binary.LittleEndian.Uint32(buffer[12:]))),
		EndLine:          int(int32(binary.LittleEndian.Uint32(buffer[16:]))),
		PathOffset:       int64(binary.LittleEndian.Uint64(buffer[24:])),
		PathLength:       int(binary.LittleEndian.Uint32(buffer[32:])),
		ContentOffset:    int64(binary.LittleEndian.Uint64(buffer[40:])),
		ContentLength:    int(binary.LittleEndian.Uint32(buffer[48:])),
		RawContentLength: int(binary.LittleEndian.Uint32(buffer[52:])),
	}, nil
}

func validateStoreHNSW(path string, nodes int) error {
	index, err := aikitann.LoadHNSWMmap(path)
	if err != nil {
		return err
	}
	actualNodes := index.Len()
	if err := index.Close(); err != nil {
		return err
	}
	if actualNodes != nodes {
		return fmt.Errorf("vector index has %d nodes, expected %d", actualNodes, nodes)
	}
	return nil
}

func repairStoreSegmentIndex(ctx context.Context, catalog storeCatalog, segmentNumber int) (bool, error) {
	if segmentNumber < 0 || segmentNumber >= len(catalog.Segments) {
		return false, fmt.Errorf("standalone segment %d is out of range", segmentNumber)
	}
	catalogPath := projectCatalogPath(filepath.Dir(catalog.Directory), catalog.ProjectRoot)
	buildLock := storeBuildLock(catalogPath)
	buildLock.Lock()
	defer buildLock.Unlock()

	release, err := lock.File(ctx, catalogPath+".lock")
	if err != nil {
		return false, fmt.Errorf("lock standalone segment repair: %w", err)
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return false, err
	}

	segment := catalog.Segments[segmentNumber]
	indexPath := filepath.Join(catalog.Directory, segment.Name, "index.hnsw")
	if err := validateStoreHNSW(indexPath, segment.Nodes); err == nil {
		return false, nil
	}
	if err := validateStoreSegment(catalog.Directory, segment); err != nil {
		return false, fmt.Errorf("validate standalone segment sidecars: %w", err)
	}

	vectorsFile, err := os.Open(filepath.Join(catalog.Directory, segment.Name, "vectors.f32"))
	if err != nil {
		return false, fmt.Errorf("open standalone segment vectors: %w", err)
	}
	defer vectorsFile.Close()
	vectors := make([][]float32, segment.Nodes)
	encodedVector := make([]byte, segment.Dimension*4)
	for position := range vectors {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, err := io.ReadFull(vectorsFile, encodedVector); err != nil {
			return false, fmt.Errorf("read standalone segment vector %d: %w", position, err)
		}
		vector, err := decodeEmbedding(encodedVector)
		if err != nil {
			return false, fmt.Errorf("decode standalone segment vector %d: %w", position, err)
		}
		vectors[position] = vector
	}
	if err := vectorsFile.Close(); err != nil {
		return false, fmt.Errorf("close standalone segment vectors: %w", err)
	}
	if err := writeStoreHNSW(indexPath, vectors); err != nil {
		return false, fmt.Errorf("write rebuilt standalone vector index: %w", err)
	}
	if err := validateStoreHNSW(indexPath, segment.Nodes); err != nil {
		return false, fmt.Errorf("validate rebuilt standalone vector index: %w", err)
	}
	return true, nil
}

func (s *segmentStore) search(ctx context.Context, query []float32, options SearchOptions) ([]SearchResult, error) {
	candidateLimit := max(options.Limit*20, 200)
	selected := s.selectedSegments(options.PathPrefix)
	if len(selected) == 0 {
		return nil, nil
	}
	maxCandidates := 0
	for _, segmentNumber := range selected {
		maxCandidates += s.catalog.Segments[segmentNumber].Nodes
	}
	candidateLimit = min(candidateLimit, maxCandidates)

	for {
		candidates, err := s.searchCandidates(ctx, query, selected, candidateLimit)
		if err != nil {
			return nil, err
		}
		results, err := s.loadResults(ctx, query, candidates, options)
		if err != nil {
			return nil, err
		}
		if len(results) >= options.Limit || candidateLimit >= maxCandidates {
			if len(results) > options.Limit {
				results = results[:options.Limit]
			}
			return results, nil
		}
		candidateLimit = min(candidateLimit*4, maxCandidates)
	}
}

func (s *segmentStore) selectedSegments(pathPrefix string) []int {
	selected := make([]int, 0, len(s.catalog.Segments))
	for index, segment := range s.catalog.Segments {
		if pathPrefixesOverlap(segment.Prefix, pathPrefix) && pathRangeOverlapsPrefix(segment.FirstPath, segment.LastPath, pathPrefix) {
			selected = append(selected, index)
		}
	}
	return selected
}

func (s *segmentStore) searchCandidates(ctx context.Context, query []float32, segments []int, count int) ([]storeCandidate, error) {
	type segmentHits struct {
		segment int
		hits    []aikitann.Hit
		err     error
	}

	workerCount := min(len(segments), max(1, runtime.GOMAXPROCS(0)))
	jobs := make(chan int)
	results := make(chan segmentHits, len(segments))
	searchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for segmentNumber := range jobs {
				if searchCtx.Err() != nil {
					return
				}
				segment := s.catalog.Segments[segmentNumber]
				indexPath := filepath.Join(s.catalog.Directory, segment.Name, "index.hnsw")
				index, err := aikitann.LoadHNSWMmap(indexPath)
				if err == nil && index.Len() != segment.Nodes {
					actualNodes := index.Len()
					_ = index.Close()
					index = nil
					err = fmt.Errorf("vector index has %d nodes, expected %d", actualNodes, segment.Nodes)
				}
				if err != nil {
					loadErr := err
					repaired, repairErr := repairStoreSegmentIndex(searchCtx, s.catalog, segmentNumber)
					if repairErr != nil {
						results <- segmentHits{segment: segmentNumber, err: fmt.Errorf("load standalone segment %d: %w; repair failed: %v", segmentNumber, loadErr, repairErr)}
						cancel()
						return
					}
					if repaired {
						slog.Warn("Rebuilt malformed codebase search segment", "project", s.catalog.ProjectRoot, "segment", segment.Name, "error", loadErr)
					}
					index, err = aikitann.LoadHNSWMmap(indexPath)
					if err == nil && index.Len() != segment.Nodes {
						actualNodes := index.Len()
						_ = index.Close()
						index = nil
						err = fmt.Errorf("vector index has %d nodes, expected %d", actualNodes, segment.Nodes)
					}
					if err != nil {
						results <- segmentHits{segment: segmentNumber, err: fmt.Errorf("load repaired standalone segment %d: %w", segmentNumber, err)}
						cancel()
						return
					}
				}
				candidateCount := min(count, index.Len())
				hits := index.QueryEf(query, candidateCount, storeSearchEF(candidateCount, index.Len()))
				if err := index.Close(); err != nil {
					results <- segmentHits{segment: segmentNumber, err: fmt.Errorf("close standalone segment %d: %w", segmentNumber, err)}
					cancel()
					return
				}
				results <- segmentHits{segment: segmentNumber, hits: hits}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, segmentNumber := range segments {
			select {
			case jobs <- segmentNumber:
			case <-searchCtx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	candidates := make(storeCandidateHeap, 0, count)
	heap.Init(&candidates)
	for result := range results {
		if result.err != nil {
			return nil, result.err
		}
		for _, hit := range result.hits {
			candidate := storeCandidate{segment: result.segment, index: hit.Index, score: hit.Score}
			if candidates.Len() < count {
				heap.Push(&candidates, candidate)
			} else if candidate.score > candidates[0].score {
				heap.Pop(&candidates)
				heap.Push(&candidates, candidate)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := make([]storeCandidate, candidates.Len())
	for position := len(values) - 1; position >= 0; position-- {
		values[position] = heap.Pop(&candidates).(storeCandidate)
	}
	return values, nil
}

func storeSearchEF(candidateCount, segmentNodes int) int {
	return min(segmentNodes, max(storeSearchMinimumEF, candidateCount*storeSearchEFMultiple))
}

func (s *segmentStore) loadResults(ctx context.Context, query []float32, candidates []storeCandidate, options SearchOptions) ([]SearchResult, error) {
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()

	segmentOrder := make([]int, 0)
	segmentCandidates := make(map[int][]storeCandidate)
	for _, candidate := range candidates {
		if _, exists := segmentCandidates[candidate.segment]; !exists {
			segmentOrder = append(segmentOrder, candidate.segment)
		}
		segmentCandidates[candidate.segment] = append(segmentCandidates[candidate.segment], candidate)
	}

	results := make([]SearchResult, 0, len(candidates))
	for _, segmentNumber := range segmentOrder {
		segmentResults, err := s.loadSegmentResults(ctx, decoder, query, segmentNumber, segmentCandidates[segmentNumber], options)
		if err != nil {
			return nil, err
		}
		results = append(results, segmentResults...)
	}
	sortSearchResults(results)
	return results, nil
}

func (s *segmentStore) loadSegmentResults(ctx context.Context, decoder *zstd.Decoder, query []float32, segmentNumber int, candidates []storeCandidate, options SearchOptions) ([]SearchResult, error) {
	segment := s.catalog.Segments[segmentNumber]
	segmentDirectory := filepath.Join(s.catalog.Directory, segment.Name)
	recordsFile, err := os.Open(filepath.Join(segmentDirectory, "records.bin"))
	if err != nil {
		return nil, err
	}
	defer recordsFile.Close()
	dataFile, err := os.Open(filepath.Join(segmentDirectory, "data.bin"))
	if err != nil {
		return nil, err
	}
	defer dataFile.Close()
	vectorsFile, err := os.Open(filepath.Join(segmentDirectory, "vectors.f32"))
	if err != nil {
		return nil, err
	}
	defer vectorsFile.Close()

	pathPrefix := normalizeIndexPath(options.PathPrefix)
	results := make([]SearchResult, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := readStoreRecord(recordsFile, candidate.index)
		if err != nil {
			return nil, fmt.Errorf("read standalone candidate record: %w", err)
		}
		pathBytes := make([]byte, record.PathLength)
		if _, err := dataFile.ReadAt(pathBytes, record.PathOffset); err != nil {
			return nil, fmt.Errorf("read standalone candidate path: %w", err)
		}
		path := string(pathBytes)
		if pathPrefix != "" && !strings.HasPrefix(normalizeIndexPath(path), pathPrefix) {
			continue
		}
		compressedContent := make([]byte, record.ContentLength)
		if _, err := dataFile.ReadAt(compressedContent, record.ContentOffset); err != nil {
			return nil, fmt.Errorf("read standalone candidate content: %w", err)
		}
		content, err := decoder.DecodeAll(compressedContent, make([]byte, 0, record.RawContentLength))
		if err != nil {
			return nil, fmt.Errorf("decode standalone candidate content: %w", err)
		}
		vectorBytes := make([]byte, s.catalog.Dimension*4)
		if _, err := vectorsFile.ReadAt(vectorBytes, int64(candidate.index*s.catalog.Dimension*4)); err != nil {
			return nil, fmt.Errorf("read standalone candidate vector: %w", err)
		}
		vector, err := decodeEmbedding(vectorBytes)
		if err != nil {
			return nil, err
		}
		score := dotProduct(query, vector)
		if score < options.MinScore {
			continue
		}
		results = append(results, SearchResult{
			Chunk: Chunk{
				ID:             record.ID,
				ProjectRoot:    s.catalog.ProjectRoot,
				Path:           path,
				ChunkIndex:     record.ChunkIndex,
				Content:        string(content),
				EmbeddingModel: s.catalog.Model,
				StartLine:      record.StartLine,
				EndLine:        record.EndLine,
			},
			Score: score,
		})
	}
	return results, nil
}

func sortSearchResults(results []SearchResult) {
	for index := 1; index < len(results); index++ {
		for position := index; position > 0 && betterSearchResult(results[position], results[position-1]); position-- {
			results[position], results[position-1] = results[position-1], results[position]
		}
	}
}

func topLevelPathPrefix(path string) string {
	path = normalizeIndexPath(path)
	if separator := strings.IndexByte(path, '/'); separator >= 0 {
		return path[:separator+1]
	}
	return ""
}

func normalizeIndexPath(path string) string {
	path = strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	path = strings.TrimPrefix(path, "./")
	return strings.TrimPrefix(path, "/")
}

func storeBuildPathRange(chunks []storeBuildChunk) (string, string) {
	firstPath := normalizeIndexPath(chunks[0].chunk.Path)
	lastPath := firstPath
	for _, value := range chunks[1:] {
		path := normalizeIndexPath(value.chunk.Path)
		firstPath = min(firstPath, path)
		lastPath = max(lastPath, path)
	}
	return firstPath, lastPath
}

func pathPrefixesOverlap(segmentPrefix, requestedPrefix string) bool {
	if requestedPrefix == "" {
		return true
	}
	segmentPrefix = normalizeIndexPath(segmentPrefix)
	requestedPrefix = normalizeIndexPath(requestedPrefix)
	if segmentPrefix == "" {
		return !strings.Contains(requestedPrefix, "/")
	}
	return strings.HasPrefix(requestedPrefix, segmentPrefix) || strings.HasPrefix(segmentPrefix, requestedPrefix)
}

func pathRangeOverlapsPrefix(firstPath, lastPath, requestedPrefix string) bool {
	if requestedPrefix == "" || firstPath == "" || lastPath == "" {
		return true
	}
	firstPath = normalizeIndexPath(firstPath)
	lastPath = normalizeIndexPath(lastPath)
	requestedPrefix = normalizeIndexPath(requestedPrefix)
	return lastPath >= requestedPrefix && firstPath <= requestedPrefix+"\xff"
}

func projectCatalogPath(directory, projectRoot string) string {
	digest := sha256.Sum256([]byte(projectRoot))
	return filepath.Join(directory, "project-"+hex.EncodeToString(digest[:8])+".catalog.json")
}

func storeGenerationDirectory(directory, projectRoot, model string, source storeSource) string {
	encoded, _ := json.Marshal(source)
	generationKey := fmt.Sprintf("%d\x00%s\x00%s\x00", storeCatalogVersion, projectRoot, model)
	digest := sha256.Sum256(append([]byte(generationKey), encoded...))
	return filepath.Join(directory, "generation-"+hex.EncodeToString(digest[:8]))
}

func loadProjectCatalog(directory, projectRoot string) (storeCatalog, error) {
	catalog, err := loadStoreCatalog(projectCatalogPath(directory, projectRoot))
	if err != nil {
		return storeCatalog{}, err
	}
	if !catalog.Complete || catalog.ProjectRoot != projectRoot {
		return storeCatalog{}, fmt.Errorf("standalone codebase search catalog does not match project")
	}
	return catalog, nil
}

func loadLatestProjectCheckpoint(directory, projectRoot, configuredDatabasePath, filter string) (storeCatalog, error) {
	paths, err := filepath.Glob(filepath.Join(directory, "generation-*", "migration.json"))
	if err != nil {
		return storeCatalog{}, err
	}
	var selected storeCatalog
	var selectedAt time.Time
	for _, path := range paths {
		catalog, err := loadStoreCatalog(path)
		if err != nil || catalog.Complete || catalog.ProjectRoot != projectRoot || catalog.Source.FilterDigest != filter {
			continue
		}
		if filepath.Clean(catalog.Directory) != filepath.Clean(filepath.Dir(path)) {
			continue
		}
		if configuredDatabasePath != "" && catalog.Source.DatabasePath != "" {
			configured, err := filepath.Abs(configuredDatabasePath)
			if err != nil || filepath.Clean(configured) != filepath.Clean(catalog.Source.DatabasePath) {
				continue
			}
		}
		if !sourceFilesCurrent(catalog.Source) {
			continue
		}
		updatedAt := catalog.ProgressUpdatedAt
		if updatedAt.IsZero() {
			if info, err := os.Stat(path); err == nil {
				updatedAt = info.ModTime()
			}
		}
		if selected.ProjectRoot == "" || updatedAt.After(selectedAt) {
			selected = catalog
			selectedAt = updatedAt
		}
	}
	if selected.ProjectRoot == "" {
		return storeCatalog{}, os.ErrNotExist
	}
	return selected, nil
}

func loadLatestNativeCheckpoint(directory, projectRoot, model, filter string) (storeCatalog, error) {
	paths, err := filepath.Glob(filepath.Join(directory, "generation-*", "migration.json"))
	if err != nil {
		return storeCatalog{}, err
	}
	var selected storeCatalog
	var selectedAt time.Time
	for _, path := range paths {
		catalog, err := loadStoreCatalog(path)
		if err != nil || catalog.ProjectRoot != projectRoot || catalog.Model != model || catalog.Source.Mode != "native" || catalog.Source.FilterDigest != filter || len(catalog.NativeFiles) == 0 {
			continue
		}
		if filepath.Clean(catalog.Directory) != filepath.Clean(filepath.Dir(path)) {
			continue
		}
		valid := true
		for _, segment := range catalog.Segments {
			if err := validateStoreSegment(catalog.Directory, segment); err != nil {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		updatedAt := catalog.ProgressUpdatedAt
		if updatedAt.IsZero() {
			if info, err := os.Stat(path); err == nil {
				updatedAt = info.ModTime()
			}
		}
		if selected.ProjectRoot == "" || updatedAt.After(selectedAt) {
			selected = catalog
			selectedAt = updatedAt
		}
	}
	if selected.ProjectRoot == "" {
		return storeCatalog{}, os.ErrNotExist
	}
	return selected, nil
}

func loadStoreCatalog(path string) (storeCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return storeCatalog{}, err
	}
	var catalog storeCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return storeCatalog{}, err
	}
	if catalog.Version != storeCatalogVersion || catalog.ProjectRoot == "" || catalog.Model == "" || catalog.Directory == "" {
		return storeCatalog{}, fmt.Errorf("standalone codebase search catalog is invalid")
	}
	if catalog.Complete {
		if catalog.Dimension <= 0 || catalog.Chunks <= 0 || len(catalog.Segments) == 0 {
			return storeCatalog{}, fmt.Errorf("standalone codebase search catalog is incomplete")
		}
		totalNodes := 0
		segmentNames := make(map[string]struct{}, len(catalog.Segments))
		for _, segment := range catalog.Segments {
			if segment.Nodes > storeSegmentSize || segment.Dimension != catalog.Dimension {
				return storeCatalog{}, fmt.Errorf("standalone segment manifest exceeds catalog bounds")
			}
			if filepath.Base(segment.Name) != segment.Name {
				return storeCatalog{}, fmt.Errorf("standalone segment manifest has unsafe name")
			}
			if _, exists := segmentNames[segment.Name]; exists {
				return storeCatalog{}, fmt.Errorf("standalone segment manifest has duplicate name")
			}
			segmentNames[segment.Name] = struct{}{}
			totalNodes += segment.Nodes
			if err := validateStoreSegment(catalog.Directory, segment); err != nil {
				return storeCatalog{}, err
			}
		}
		if totalNodes != catalog.Chunks {
			return storeCatalog{}, fmt.Errorf("standalone segment node count does not match catalog")
		}
		upgraded, err := populateStoreSegmentPathRanges(&catalog)
		if err != nil {
			return storeCatalog{}, err
		}
		if upgraded {
			if err := writeJSONAtomically(path, catalog); err != nil {
				return storeCatalog{}, fmt.Errorf("upgrade standalone codebase search catalog: %w", err)
			}
		}
	}
	return catalog, nil
}

func validateStoreSegment(directory string, segment storeSegmentManifest) error {
	if segment.Name == "" || segment.Nodes <= 0 || segment.Dimension <= 0 {
		return fmt.Errorf("standalone segment manifest is invalid")
	}
	segmentDirectory := filepath.Join(directory, segment.Name)
	files := []struct {
		name string
		size int64
	}{
		{name: "index.hnsw", size: 0},
		{name: "vectors.f32", size: int64(segment.Nodes * segment.Dimension * 4)},
		{name: "records.bin", size: int64(segment.Nodes * storeRecordSize)},
		{name: "data.bin", size: 0},
	}
	for _, expected := range files {
		info, err := os.Stat(filepath.Join(segmentDirectory, expected.name))
		if err != nil {
			return err
		}
		if expected.name == "index.hnsw" && info.Size() == 0 || expected.size > 0 && info.Size() != expected.size {
			return fmt.Errorf("standalone segment %q has invalid %s size", segment.Name, expected.name)
		}
	}
	return nil
}

func populateStoreSegmentPathRanges(catalog *storeCatalog) (bool, error) {
	upgraded := false
	for index := range catalog.Segments {
		segment := &catalog.Segments[index]
		if segment.FirstPath != "" && segment.LastPath != "" {
			continue
		}
		firstPath, lastPath, err := readStoreSegmentPathRange(catalog.Directory, *segment)
		if err != nil {
			return false, fmt.Errorf("read standalone segment %q path range: %w", segment.Name, err)
		}
		segment.FirstPath = firstPath
		segment.LastPath = lastPath
		upgraded = true
	}
	return upgraded, nil
}

func readStoreSegmentPathRange(directory string, segment storeSegmentManifest) (string, string, error) {
	segmentDirectory := filepath.Join(directory, segment.Name)
	recordsFile, err := os.Open(filepath.Join(segmentDirectory, "records.bin"))
	if err != nil {
		return "", "", err
	}
	defer recordsFile.Close()
	dataFile, err := os.Open(filepath.Join(segmentDirectory, "data.bin"))
	if err != nil {
		return "", "", err
	}
	defer dataFile.Close()

	readPath := func(position int) (string, error) {
		record, err := readStoreRecord(recordsFile, position)
		if err != nil {
			return "", err
		}
		if record.PathLength <= 0 {
			return "", fmt.Errorf("record %d has an empty path", position)
		}
		pathBytes := make([]byte, record.PathLength)
		if _, err := dataFile.ReadAt(pathBytes, record.PathOffset); err != nil {
			return "", err
		}
		return normalizeIndexPath(string(pathBytes)), nil
	}
	firstPath, err := readPath(0)
	if err != nil {
		return "", "", err
	}
	lastPath, err := readPath(segment.Nodes - 1)
	if err != nil {
		return "", "", err
	}
	return firstPath, lastPath, nil
}

func (c storeCatalog) matches(projectRoot, model string, source storeSource) bool {
	return c.Complete && c.partialMatches(projectRoot, model, source, c.Directory)
}

func (c storeCatalog) partialMatches(projectRoot, model string, source storeSource, directory string) bool {
	return c.Version == storeCatalogVersion &&
		c.ProjectRoot == projectRoot &&
		c.Model == model &&
		c.Source == source &&
		c.Directory == directory
}

func writeJSONAtomically(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeFileAtomically(path, func(writer io.Writer) error {
		_, err := writer.Write(data)
		return err
	})
}

func writeFileAtomically(path string, write func(io.Writer) error) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".codebase-store-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if err := write(temporary); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
