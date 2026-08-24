package codebaseindex

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/semanticembedding"
	"github.com/klauspost/compress/zstd"
)

const nativeIndexMaxFileSize int64 = 8 << 20

var (
	nativeCheckpointFileInterval = 250
	nativeCheckpointTimeInterval = 5 * time.Second
)

var nativeIndexExtensions = map[string]struct{}{
	".c": {}, ".cc": {}, ".cpp": {}, ".cs": {}, ".css": {}, ".go": {}, ".h": {}, ".hpp": {},
	".html": {}, ".java": {}, ".js": {}, ".json": {}, ".jsx": {}, ".kt": {}, ".kts": {}, ".md": {},
	".mjs": {}, ".php": {}, ".py": {}, ".rb": {}, ".rs": {}, ".scala": {}, ".scss": {}, ".sh": {},
	".sql": {}, ".svelte": {}, ".swift": {}, ".toml": {}, ".ts": {}, ".tsx": {}, ".vue": {}, ".yaml": {}, ".yml": {},
}

type nativeProjectFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

type documentChunkEmbedder = semanticembedding.DocumentEmbedder

type nativeStoreChunkIterator struct {
	catalog     storeCatalog
	decoder     *zstd.Decoder
	segment     int
	position    int
	recordsFile *os.File
	dataFile    *os.File
	vectorsFile *os.File
	pending     *storeBuildChunk
}

func newNativeStoreChunkIterator(catalog storeCatalog) (*nativeStoreChunkIterator, error) {
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &nativeStoreChunkIterator{catalog: catalog, decoder: decoder}, nil
}

func (i *nativeStoreChunkIterator) Close() error {
	var firstErr error
	for _, file := range []*os.File{i.recordsFile, i.dataFile, i.vectorsFile} {
		if file != nil {
			if err := file.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	i.decoder.Close()
	return firstErr
}

func (i *nativeStoreChunkIterator) chunksForPath(ctx context.Context, path string) ([]storeBuildChunk, error) {
	chunks := make([]storeBuildChunk, 0)
	for {
		var chunk storeBuildChunk
		var ok bool
		var err error
		if i.pending != nil {
			chunk = *i.pending
			i.pending = nil
			ok = true
		} else {
			chunk, ok, err = i.next(ctx)
			if err != nil {
				return nil, err
			}
		}
		if !ok {
			return chunks, nil
		}
		if chunk.chunk.Path < path {
			continue
		}
		if chunk.chunk.Path > path {
			i.pending = &chunk
			return chunks, nil
		}
		chunks = append(chunks, chunk)
	}
}

func (i *nativeStoreChunkIterator) next(ctx context.Context) (storeBuildChunk, bool, error) {
	for i.segment < len(i.catalog.Segments) {
		if err := ctx.Err(); err != nil {
			return storeBuildChunk{}, false, err
		}
		segment := i.catalog.Segments[i.segment]
		if i.recordsFile == nil {
			segmentDirectory := filepath.Join(i.catalog.Directory, segment.Name)
			var err error
			i.recordsFile, err = os.Open(filepath.Join(segmentDirectory, "records.bin"))
			if err != nil {
				return storeBuildChunk{}, false, err
			}
			i.dataFile, err = os.Open(filepath.Join(segmentDirectory, "data.bin"))
			if err != nil {
				return storeBuildChunk{}, false, err
			}
			i.vectorsFile, err = os.Open(filepath.Join(segmentDirectory, "vectors.f32"))
			if err != nil {
				return storeBuildChunk{}, false, err
			}
		}
		if i.position >= segment.Nodes {
			if err := i.closeSegment(); err != nil {
				return storeBuildChunk{}, false, err
			}
			i.segment++
			i.position = 0
			continue
		}

		record, err := readStoreRecord(i.recordsFile, i.position)
		if err != nil {
			return storeBuildChunk{}, false, fmt.Errorf("read reusable codebase index record: %w", err)
		}
		pathBytes := make([]byte, record.PathLength)
		if _, err := i.dataFile.ReadAt(pathBytes, record.PathOffset); err != nil {
			return storeBuildChunk{}, false, fmt.Errorf("read reusable codebase index path: %w", err)
		}
		compressedContent := make([]byte, record.ContentLength)
		if _, err := i.dataFile.ReadAt(compressedContent, record.ContentOffset); err != nil {
			return storeBuildChunk{}, false, fmt.Errorf("read reusable codebase index content: %w", err)
		}
		content, err := i.decoder.DecodeAll(compressedContent, make([]byte, 0, record.RawContentLength))
		if err != nil {
			return storeBuildChunk{}, false, fmt.Errorf("decode reusable codebase index content: %w", err)
		}
		vectorBytes := make([]byte, segment.Dimension*4)
		if _, err := i.vectorsFile.ReadAt(vectorBytes, int64(i.position*segment.Dimension*4)); err != nil {
			return storeBuildChunk{}, false, fmt.Errorf("read reusable codebase index vector: %w", err)
		}
		vector, err := decodeEmbedding(vectorBytes)
		if err != nil {
			return storeBuildChunk{}, false, err
		}
		i.position++
		return storeBuildChunk{
			chunk: Chunk{
				ID:             record.ID,
				ProjectRoot:    i.catalog.ProjectRoot,
				Path:           string(pathBytes),
				ChunkIndex:     record.ChunkIndex,
				Content:        string(content),
				EmbeddingModel: i.catalog.Model,
				StartLine:      record.StartLine,
				EndLine:        record.EndLine,
			},
			vector: vector,
		}, true, nil
	}
	return storeBuildChunk{}, false, nil
}

func (i *nativeStoreChunkIterator) closeSegment() error {
	var firstErr error
	for _, file := range []*os.File{i.recordsFile, i.dataFile, i.vectorsFile} {
		if file != nil {
			if err := file.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	i.recordsFile = nil
	i.dataFile = nil
	i.vectorsFile = nil
	return firstErr
}

func buildNativeProjectStore(ctx context.Context, projectRoot, storeDirectory string, filters ProjectFilters, embedder documentChunkEmbedder, report func(IndexProgress)) error {
	if embedder == nil {
		return fmt.Errorf("document chunk embedder is nil")
	}
	projectRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return fmt.Errorf("resolve native index project root: %w", err)
	}
	storeDirectory, err = resolveStoreDirectory(storeDirectory)
	if err != nil {
		return err
	}
	filters = NormalizeProjectFilters(filters)
	files, source, err := nativeProjectSource(ctx, projectRoot, filters)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no indexable project files matched the configured path filters")
	}
	if report != nil {
		report(IndexProgress{Stage: "Preparing files", FilesTotal: len(files)})
	}
	model := embedder.PreferredEmbeddingModel(ctx)
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("GitHub returned an empty embedding model")
	}

	catalogPath := projectCatalogPath(storeDirectory, projectRoot)
	generationDirectory := storeGenerationDirectory(storeDirectory, projectRoot, model, source)
	buildLock := storeBuildLock(catalogPath)
	buildLock.Lock()
	defer buildLock.Unlock()
	release, err := lock.File(ctx, catalogPath+".lock")
	if err != nil {
		return fmt.Errorf("lock native codebase indexing: %w", err)
	}
	defer release()

	previousCatalog, previousCatalogErr := loadStoreCatalog(catalogPath)
	if previousCatalogErr == nil && previousCatalog.matches(projectRoot, model, source) {
		return nil
	}
	if err := os.MkdirAll(generationDirectory, 0o700); err != nil {
		return fmt.Errorf("create native codebase index generation: %w", err)
	}
	checkpointPath := filepath.Join(generationDirectory, "migration.json")
	catalog, err := loadStoreCatalog(checkpointPath)
	exactResume := err == nil && catalog.partialMatches(projectRoot, model, source, generationDirectory)
	if !exactResume {
		catalog = storeCatalog{
			Version:           storeCatalogVersion,
			ProjectRoot:       projectRoot,
			Model:             model,
			Directory:         generationDirectory,
			Source:            source,
			Stage:             "Preparing files",
			StartedAt:         time.Now(),
			ProgressUpdatedAt: time.Now(),
		}
	}
	if catalog.StartedAt.IsZero() {
		catalog.StartedAt = time.Now()
	}
	catalog.NativeFiles = append([]nativeProjectFile(nil), files...)

	reuseCatalog := previousCatalog
	reuseCatalogAvailable := previousCatalogErr == nil &&
		previousCatalog.Complete &&
		previousCatalog.ProjectRoot == projectRoot &&
		previousCatalog.Model == model &&
		previousCatalog.Source.Mode == "native" &&
		len(previousCatalog.NativeFiles) > 0
	if !reuseCatalogAvailable && !exactResume {
		reuseCatalog, err = loadLatestNativeCheckpoint(storeDirectory, projectRoot, model, filterDigest(filters))
		if err == nil {
			reuseCatalogAvailable = filepath.Clean(reuseCatalog.Directory) != filepath.Clean(generationDirectory)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("load reusable native codebase index checkpoint: %w", err)
		}
	}

	previousFiles := make(map[string]nativeProjectFile)
	previousSkipped := make(map[string]struct{})
	var reusableChunks *nativeStoreChunkIterator
	if reuseCatalogAvailable {
		for _, file := range reuseCatalog.NativeFiles {
			previousFiles[file.Path] = file
		}
		for _, path := range reuseCatalog.NativeSkipped {
			previousSkipped[path] = struct{}{}
		}
		if len(reuseCatalog.Segments) > 0 {
			reusableChunks, err = newNativeStoreChunkIterator(reuseCatalog)
			if err != nil {
				return fmt.Errorf("open reusable native codebase index: %w", err)
			}
			defer reusableChunks.Close()
		}
	}

	chunks := make([]storeBuildChunk, 0, min(storeBuildSegmentSize, 5000))
	filesProcessed := 0
	filesSkipped := catalog.FilesSkipped
	catalog.FilesTotal = len(files)
	skippedPaths := make(map[string]struct{}, len(catalog.NativeSkipped))
	for _, path := range catalog.NativeSkipped {
		skippedPaths[path] = struct{}{}
	}
	markSkipped := func(path string) {
		if _, exists := skippedPaths[path]; exists {
			return
		}
		filesSkipped++
		catalog.NativeSkipped = append(catalog.NativeSkipped, path)
		skippedPaths[path] = struct{}{}
	}
	for _, file := range files {
		if (catalog.LastCompletedPath != "" && file.Path <= catalog.LastCompletedPath) || (catalog.LastCompletedPath == "" && file.Path < catalog.LastPath) {
			filesProcessed++
		}
	}
	catalog.FilesProcessed = filesProcessed
	catalog.FilesSkipped = filesSkipped
	catalog.Stage = "Indexing files"
	catalog.ProgressUpdatedAt = time.Now()
	if err := writeJSONAtomically(checkpointPath, catalog); err != nil {
		return fmt.Errorf("initialize native codebase index checkpoint: %w", err)
	}
	latestCompletedPath := catalog.LastCompletedPath
	filesSinceCheckpoint := 0
	lastCheckpointAt := time.Now()
	currentPath := catalog.CurrentPath
	reportProgress := func(stage, path string) {
		currentPath = path
		if report != nil {
			report(IndexProgress{
				Stage:          stage,
				FilesTotal:     len(files),
				FilesProcessed: filesProcessed,
				ChunksCreated:  catalog.Chunks + len(chunks),
				FilesSkipped:   filesSkipped,
				CurrentPath:    path,
			})
		}
	}
	persist := func(stage string) error {
		catalog.FilesTotal = len(files)
		catalog.FilesProcessed = filesProcessed
		catalog.FilesSkipped = filesSkipped
		catalog.LastCompletedPath = latestCompletedPath
		catalog.CurrentPath = currentPath
		catalog.Stage = stage
		catalog.ProgressUpdatedAt = time.Now()
		if err := writeJSONAtomically(checkpointPath, catalog); err != nil {
			return fmt.Errorf("checkpoint native codebase indexing: %w", err)
		}
		filesSinceCheckpoint = 0
		lastCheckpointAt = time.Now()
		return nil
	}
	prefix := ""
	flush := func() error {
		if len(chunks) > 0 {
			segment, err := saveStoreSegment(generationDirectory, len(catalog.Segments), prefix, catalog.Dimension, chunks)
			if err != nil {
				return err
			}
			last := chunks[len(chunks)-1].chunk
			catalog.Segments = append(catalog.Segments, segment)
			catalog.Chunks += len(chunks)
			catalog.LastPath = last.Path
			catalog.LastChunkIndex = last.ChunkIndex
			catalog.LastChunkID = last.ID
			chunks = make([]storeBuildChunk, 0, min(storeBuildSegmentSize, 5000))
		}
		if err := persist("Indexing files"); err != nil {
			return err
		}
		reportProgress("Indexing files", currentPath)
		return nil
	}
	checkpointCompletedFile := func(path string) error {
		latestCompletedPath = path
		filesSinceCheckpoint++
		if filesSinceCheckpoint >= nativeCheckpointFileInterval || time.Since(lastCheckpointAt) >= nativeCheckpointTimeInterval {
			return flush()
		}
		return nil
	}
	completeFile := func(path string, skipped bool) error {
		filesProcessed++
		if skipped {
			markSkipped(path)
		}
		reportProgress("Indexing files", path)
		return checkpointCompletedFile(path)
	}
	stop := func(cause error) error {
		if err := flush(); err != nil {
			return err
		}
		return cause
	}

	reportProgress("Indexing files", "")
	segmentLimit := min(storeBuildSegmentSize, 5000)
	appendChunk := func(value storeBuildChunk) error {
		if value.chunk.Path == catalog.LastPath && value.chunk.ChunkIndex <= catalog.LastChunkIndex {
			return nil
		}
		if catalog.Dimension == 0 {
			catalog.Dimension = len(value.vector)
		} else if len(value.vector) != catalog.Dimension {
			return fmt.Errorf("project file %q embedding dimension %d does not match model %q dimension %d", value.chunk.Path, len(value.vector), model, catalog.Dimension)
		}
		chunkPrefix := topLevelPathPrefix(value.chunk.Path)
		if len(chunks) > 0 && (len(chunks) >= segmentLimit || chunkPrefix != prefix) {
			if err := flush(); err != nil {
				return err
			}
		}
		if len(chunks) == 0 {
			prefix = chunkPrefix
		}
		chunks = append(chunks, value)
		return nil
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return stop(err)
		}
		if (catalog.LastCompletedPath != "" && file.Path <= catalog.LastCompletedPath) || (catalog.LastCompletedPath == "" && file.Path < catalog.LastPath) {
			continue
		}
		reportProgress("Indexing files", file.Path)
		if previousFile, exists := previousFiles[file.Path]; exists && previousFile == file {
			if _, skipped := previousSkipped[file.Path]; skipped {
				if err := completeFile(file.Path, true); err != nil {
					return err
				}
				continue
			}
			var reused []storeBuildChunk
			if reusableChunks != nil {
				reused, err = reusableChunks.chunksForPath(ctx, file.Path)
				if err != nil {
					return stop(fmt.Errorf("reuse project file %q: %w", file.Path, err))
				}
			}
			if len(reused) > 0 {
				for _, value := range reused {
					value.chunk.Mtime = file.ModTime
					if err := appendChunk(value); err != nil {
						return stop(err)
					}
				}
				if err := completeFile(file.Path, false); err != nil {
					return err
				}
				continue
			}
		}

		content, err := readNativeProjectFile(projectRoot, file)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if err := completeFile(file.Path, true); err != nil {
					return err
				}
				continue
			}
			return stop(err)
		}
		if content == "" {
			if err := completeFile(file.Path, true); err != nil {
				return err
			}
			continue
		}
		embedded, err := embedder.ChunkAndEmbedFile(ctx, file.Path, content, model)
		if err != nil {
			var semanticErr *GitHubSemanticError
			if errors.As(err, &semanticErr) && semanticErr.Status == 413 {
				if err := completeFile(file.Path, true); err != nil {
					return err
				}
				continue
			}
			return stop(fmt.Errorf("index project file %q: %w", file.Path, err))
		}
		for chunkIndex, embeddedChunk := range embedded {
			if embeddedChunk.Model != model {
				return stop(fmt.Errorf("project file %q chunk model %q does not match %q", file.Path, embeddedChunk.Model, model))
			}
			vector := append([]float32(nil), embeddedChunk.Embedding...)
			if len(vector) == 0 || !finiteEmbedding(vector) {
				continue
			}
			norm := vectorNorm(vector)
			if norm == 0 {
				continue
			}
			for dimension := range vector {
				vector[dimension] /= float32(norm)
			}
			if err := appendChunk(storeBuildChunk{
				chunk: Chunk{
					ID:             nativeChunkID(file.Path, embeddedChunk.Hash, chunkIndex),
					ProjectRoot:    projectRoot,
					Path:           file.Path,
					ChunkIndex:     chunkIndex,
					Content:        embeddedChunk.Text,
					EmbeddingModel: model,
					ChunkHash:      embeddedChunk.Hash,
					StartLine:      embeddedChunk.StartLine,
					EndLine:        embeddedChunk.EndLine,
					Mtime:          file.ModTime,
				},
				vector: vector,
			}); err != nil {
				return stop(err)
			}
		}
		if err := completeFile(file.Path, false); err != nil {
			return err
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if catalog.Chunks == 0 {
		return fmt.Errorf("GitHub returned no usable embeddings for the selected project files")
	}
	_, currentSource, err := nativeProjectSource(ctx, projectRoot, filters)
	if err != nil {
		return err
	}
	if currentSource != source {
		return fmt.Errorf("project files changed during native codebase indexing")
	}
	catalog.Complete = true
	catalog.FilesTotal = len(files)
	catalog.FilesProcessed = filesProcessed
	catalog.FilesSkipped = filesSkipped
	catalog.CurrentPath = ""
	catalog.Stage = "Complete"
	catalog.ProgressUpdatedAt = time.Now()
	catalog.IndexedAt = time.Now()
	if err := writeJSONAtomically(checkpointPath, catalog); err != nil {
		return fmt.Errorf("complete native codebase index checkpoint: %w", err)
	}
	if err := writeJSONAtomically(catalogPath, catalog); err != nil {
		return fmt.Errorf("activate native codebase search catalog: %w", err)
	}
	reportProgress("Complete", "")
	return nil
}

func nativeProjectSource(ctx context.Context, projectRoot string, filters ProjectFilters) ([]nativeProjectFile, storeSource, error) {
	files, err := listNativeProjectFiles(ctx, projectRoot, filters)
	if err != nil {
		return nil, storeSource{}, err
	}
	filters = NormalizeProjectFilters(filters)
	encodedFilters, err := json.Marshal(filters)
	if err != nil {
		return nil, storeSource{}, err
	}
	hash := sha256.New()
	var number [8]byte
	for _, file := range files {
		hash.Write([]byte(file.Path))
		hash.Write([]byte{0})
		binary.LittleEndian.PutUint64(number[:], uint64(file.Size))
		hash.Write(number[:])
		binary.LittleEndian.PutUint64(number[:], uint64(file.ModTime))
		hash.Write(number[:])
	}
	return files, storeSource{
		Mode:              "native",
		FilterDigest:      filterDigest(filters),
		ChunkCount:        int64(len(files)),
		NativeProjectRoot: projectRoot,
		NativeFingerprint: hex.EncodeToString(hash.Sum(nil)),
		NativeFilters:     string(encodedFilters),
	}, nil
}

func nativeSourceCurrent(source storeSource) bool {
	if source.NativeProjectRoot == "" || source.NativeFingerprint == "" {
		return false
	}
	var filters ProjectFilters
	if err := json.Unmarshal([]byte(source.NativeFilters), &filters); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, current, err := nativeProjectSource(ctx, source.NativeProjectRoot, filters)
	return err == nil && current == source
}

func listNativeProjectFiles(ctx context.Context, projectRoot string, filters ProjectFilters) ([]nativeProjectFile, error) {
	filters = NormalizeProjectFilters(filters)
	paths, err := gitProjectFiles(ctx, projectRoot)
	if err != nil {
		paths, err = walkProjectFiles(ctx, projectRoot)
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	files := make([]nativeProjectFile, 0, len(paths))
	lastPath := ""
	for _, candidate := range paths {
		path := normalizeIndexPath(candidate)
		if path == lastPath || !validNativeProjectPath(path) || !projectPathIncluded(path, filters) || !nativeIndexExtension(path) {
			continue
		}
		lastPath = path
		absolutePath := filepath.Join(projectRoot, filepath.FromSlash(path))
		info, err := os.Lstat(absolutePath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat project file %q: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > nativeIndexMaxFileSize {
			continue
		}
		files = append(files, nativeProjectFile{Path: path, Size: info.Size(), ModTime: info.ModTime().UnixNano()})
	}
	return files, nil
}

func gitProjectFiles(ctx context.Context, projectRoot string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", projectRoot, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	values := strings.Split(string(output), "\x00")
	return values[:max(0, len(values)-1)], nil
}

func walkProjectFiles(ctx context.Context, projectRoot string) ([]string, error) {
	paths := make([]string, 0)
	err := filepath.WalkDir(projectRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && path != projectRoot && (entry.Name() == ".git" || entry.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if entry.Type().IsRegular() {
			relative, err := filepath.Rel(projectRoot, path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(relative))
		}
		return nil
	})
	return paths, err
}

func validNativeProjectPath(path string) bool {
	if path == "" || path == "." || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return clean == path && clean != ".." && !strings.HasPrefix(clean, "../")
}

func nativeIndexExtension(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	if strings.HasSuffix(name, ".min.js") || strings.HasSuffix(name, ".min.css") {
		return false
	}
	_, exists := nativeIndexExtensions[strings.ToLower(filepath.Ext(name))]
	return exists
}

func readNativeProjectFile(projectRoot string, file nativeProjectFile) (string, error) {
	path := filepath.Join(projectRoot, filepath.FromSlash(file.Path))
	if !fsext.HasPrefix(path, projectRoot) {
		return "", nil
	}
	value, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer value.Close()
	info, err := value.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > nativeIndexMaxFileSize {
		return "", nil
	}
	content, err := io.ReadAll(io.LimitReader(value, nativeIndexMaxFileSize+1))
	if err != nil {
		return "", err
	}
	if int64(len(content)) > nativeIndexMaxFileSize || !utf8.Valid(content) || strings.IndexByte(string(content), 0) >= 0 {
		return "", nil
	}
	return string(content), nil
}

func nativeChunkID(path, hash string, chunkIndex int) int64 {
	digest := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d", path, hash, chunkIndex))
	value := int64(binary.LittleEndian.Uint64(digest[:8]) & math.MaxInt64)
	if value == 0 {
		return 1
	}
	return value
}
