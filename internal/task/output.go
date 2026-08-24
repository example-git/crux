package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxOutputBytes = 64 << 20
	MaxOutputBytes        = 5 << 30
	DefaultReadBytes      = 8 << 20
	MaxReadBytes          = 8 << 20
	DefaultOutputWait     = 30 * time.Second
	MaxOutputWait         = 10 * time.Minute
	DefaultCleanupLimit   = 128
)

var ErrOutputLimitExceeded = errors.New("task output limit exceeded")

type OutputStream string

const (
	OutputStreamMerged OutputStream = "merged"
	OutputStreamStdout OutputStream = "stdout"
	OutputStreamStderr OutputStream = "stderr"
)

type RetrievalStatus string

const (
	RetrievalReady    RetrievalStatus = "ready"
	RetrievalNotReady RetrievalStatus = "not_ready"
	RetrievalTimeout  RetrievalStatus = "timeout"
)

type OutputMetadata struct {
	Version         int    `json:"version"`
	TaskID          string `json:"task_id"`
	CreatedAt       int64  `json:"created_at"`
	ClosedAt        int64  `json:"closed_at,omitempty"`
	StdoutBytes     int64  `json:"stdout_bytes"`
	StderrBytes     int64  `json:"stderr_bytes"`
	MergedBytes     int64  `json:"merged_bytes"`
	OutputBytes     int64  `json:"output_bytes"`
	OutputTruncated bool   `json:"output_truncated"`
	StorageError    string `json:"storage_error,omitempty"`
}

type ReadOptions struct {
	Stream    OutputStream
	Offset    *int64
	TailBytes *int64
	MaxBytes  int64
}

type ReadResult struct {
	Output          []byte
	NextOffset      int64
	OutputTruncated bool
	Metadata        OutputMetadata
}

type OutputStoreOptions struct {
	MaxOutputBytes int64
	Now            func() time.Time
}

type OutputStore struct {
	root           string
	dir            *os.File
	maxOutputBytes int64
	now            func() time.Time
}

type Output struct {
	store        *OutputStore
	metadata     OutputMetadata
	stdout       *os.File
	stderr       *os.File
	merged       *os.File
	meta         *os.File
	mu           sync.Mutex
	closed       bool
	limitError   bool
	limitHandler func()
}

type outputWriter struct {
	output *Output
	stream OutputStream
}

func NewOutputStore(root string, options OutputStoreOptions) (*OutputStore, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("task output root must be absolute")
	}
	maxOutputBytes := options.MaxOutputBytes
	if maxOutputBytes == 0 {
		maxOutputBytes = DefaultMaxOutputBytes
	}
	if maxOutputBytes < 1 || maxOutputBytes > MaxOutputBytes {
		return nil, fmt.Errorf("task output limit must be between 1 and %d bytes", int64(MaxOutputBytes))
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating task output directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("protecting task output directory: %w", err)
	}
	dir, err := openSecureDir(root)
	if err != nil {
		return nil, fmt.Errorf("opening task output directory: %w", err)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &OutputStore{root: root, dir: dir, maxOutputBytes: maxOutputBytes, now: now}, nil
}

func (s *OutputStore) Close() error {
	if s == nil || s.dir == nil {
		return nil
	}
	return s.dir.Close()
}

func (s *OutputStore) Root() string {
	return s.root
}

func (s *OutputStore) Create(taskID string) (*Output, error) {
	taskType, err := ParseID(taskID)
	if err != nil {
		return nil, err
	}
	if taskType != TypeShell && taskType != TypeAgent {
		return nil, fmt.Errorf("unsupported task output type %q", taskType)
	}

	files := make([]*os.File, 0, 4)
	created := make([]string, 0, 4)
	cleanup := func() {
		for _, file := range files {
			_ = file.Close()
		}
		for _, name := range created {
			_ = removeSecureFile(s.dir, s.root, name)
		}
	}
	create := func(name string) (*os.File, error) {
		file, createErr := createSecureFile(s.dir, s.root, name, 0o600)
		if createErr != nil {
			return nil, createErr
		}
		files = append(files, file)
		created = append(created, name)
		return file, nil
	}

	stdout, err := create(outputName(taskID, OutputStreamStdout))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("creating stdout storage: %w", err)
	}
	stderr, err := create(outputName(taskID, OutputStreamStderr))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("creating stderr storage: %w", err)
	}
	merged, err := create(outputName(taskID, OutputStreamMerged))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("creating merged output storage: %w", err)
	}
	meta, err := create(metadataName(taskID))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("creating output metadata: %w", err)
	}

	output := &Output{
		store:  s,
		stdout: stdout,
		stderr: stderr,
		merged: merged,
		meta:   meta,
		metadata: OutputMetadata{
			Version:   1,
			TaskID:    taskID,
			CreatedAt: s.now().UnixMilli(),
		},
	}
	if err := output.persistMetadataLocked(); err != nil {
		cleanup()
		return nil, err
	}
	return output, nil
}

func (o *Output) Ref() string {
	return "task-output:" + o.metadata.TaskID
}

func (o *Output) SetLimitHandler(handler func()) {
	o.mu.Lock()
	o.limitHandler = handler
	o.mu.Unlock()
}

func (o *Output) Stdout() io.Writer {
	return outputWriter{output: o, stream: OutputStreamStdout}
}

func (o *Output) Stderr() io.Writer {
	return outputWriter{output: o, stream: OutputStreamStderr}
}

func (w outputWriter) Write(data []byte) (int, error) {
	return w.output.write(w.stream, data)
}

func (o *Output) write(stream OutputStream, data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, os.ErrClosed
	}
	if o.limitError {
		return 0, ErrOutputLimitExceeded
	}
	if o.metadata.StorageError != "" {
		return 0, errors.New(o.metadata.StorageError)
	}

	remaining := o.store.maxOutputBytes - o.metadata.OutputBytes
	accepted := int64(len(data))
	limitExceeded := accepted > remaining
	if limitExceeded {
		accepted = max(remaining, 0)
	}
	if accepted > 0 {
		streamFile := o.stdout
		if stream == OutputStreamStderr {
			streamFile = o.stderr
		}
		if stream != OutputStreamStdout && stream != OutputStreamStderr {
			return 0, fmt.Errorf("invalid output stream %q", stream)
		}
		chunk := data[:accepted]
		if err := writeFull(streamFile, chunk); err != nil {
			o.setStorageErrorLocked(err)
			return 0, err
		}
		if err := writeFull(o.merged, chunk); err != nil {
			o.setStorageErrorLocked(err)
			return int(accepted), err
		}
		if stream == OutputStreamStdout {
			o.metadata.StdoutBytes += accepted
		} else {
			o.metadata.StderrBytes += accepted
		}
		o.metadata.MergedBytes += accepted
		o.metadata.OutputBytes += accepted
	}
	if limitExceeded {
		o.metadata.OutputTruncated = true
		o.limitError = true
		if o.limitHandler != nil {
			o.limitHandler()
		}
	}
	if err := o.persistMetadataLocked(); err != nil {
		o.setStorageErrorLocked(err)
		return int(accepted), err
	}
	if limitExceeded {
		return int(accepted), ErrOutputLimitExceeded
	}
	return int(accepted), nil
}

func (o *Output) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	o.metadata.ClosedAt = o.store.now().UnixMilli()
	var closeError error
	for _, file := range []*os.File{o.stdout, o.stderr, o.merged} {
		if err := file.Sync(); err != nil && closeError == nil {
			closeError = err
		}
	}
	if closeError != nil {
		o.metadata.StorageError = closeError.Error()
	}
	if err := o.persistMetadataLocked(); err != nil && closeError == nil {
		closeError = err
	}
	for _, file := range []*os.File{o.stdout, o.stderr, o.merged, o.meta} {
		if err := file.Close(); err != nil && closeError == nil {
			closeError = err
		}
	}
	return closeError
}

func (o *Output) Metadata() OutputMetadata {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.metadata
}

func (o *Output) Read(options ReadOptions) (ReadResult, error) {
	o.mu.Lock()
	metadata := o.metadata
	o.mu.Unlock()
	return o.store.read(metadata.TaskID, metadata, options)
}

func (s *OutputStore) Read(ref string, options ReadOptions) (ReadResult, error) {
	taskID, err := parseOutputRef(ref)
	if err != nil {
		return ReadResult{}, err
	}
	metadata, err := s.readMetadata(taskID)
	if err != nil {
		return ReadResult{}, err
	}
	return s.read(taskID, metadata, options)
}

func (s *OutputStore) read(taskID string, metadata OutputMetadata, options ReadOptions) (ReadResult, error) {
	stream := options.Stream
	if stream == "" {
		stream = OutputStreamMerged
	}
	if stream != OutputStreamMerged && stream != OutputStreamStdout && stream != OutputStreamStderr {
		return ReadResult{}, fmt.Errorf("invalid output stream %q", stream)
	}
	if options.Offset != nil && options.TailBytes != nil {
		return ReadResult{}, fmt.Errorf("offset and tail_bytes are mutually exclusive")
	}
	maxBytes := options.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultReadBytes
	}
	if maxBytes < 1 || maxBytes > MaxReadBytes {
		return ReadResult{}, fmt.Errorf("max_bytes must be between 1 and %d", int64(MaxReadBytes))
	}
	file, err := openSecureFile(s.dir, s.root, outputName(taskID, stream))
	if err != nil {
		return ReadResult{}, fmt.Errorf("opening task output: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ReadResult{}, fmt.Errorf("reading task output size: %w", err)
	}
	size := info.Size()
	offset := int64(0)
	if options.Offset != nil {
		if *options.Offset < 0 {
			return ReadResult{}, fmt.Errorf("offset must not be negative")
		}
		offset = min(*options.Offset, size)
	}
	if options.TailBytes != nil {
		if *options.TailBytes < 0 {
			return ReadResult{}, fmt.Errorf("tail_bytes must not be negative")
		}
		tailBytes := min(*options.TailBytes, maxBytes)
		offset = max(size-tailBytes, 0)
	}
	length := min(max(size-offset, 0), maxBytes)
	data := make([]byte, length)
	read, err := file.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return ReadResult{}, fmt.Errorf("reading task output: %w", err)
	}
	data = data[:read]
	return ReadResult{
		Output:          data,
		NextOffset:      offset + int64(read),
		OutputTruncated: metadata.OutputTruncated,
		Metadata:        metadata,
	}, nil
}

func (s *OutputStore) Remove(taskID string) error {
	if _, err := ParseID(taskID); err != nil {
		return err
	}
	var removeError error
	for _, name := range outputNames(taskID) {
		if err := removeSecureFile(s.dir, s.root, name); err != nil && !errors.Is(err, os.ErrNotExist) && removeError == nil {
			removeError = err
		}
	}
	return removeError
}

func (s *OutputStore) Cleanup(before time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = DefaultCleanupLimit
	}
	if _, err := s.dir.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("rewinding task output directory: %w", err)
	}
	entries, err := s.dir.ReadDir(-1)
	if err != nil {
		return 0, fmt.Errorf("reading task output directory: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if removed >= limit || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		taskID := strings.TrimSuffix(entry.Name(), ".meta.json")
		if _, err := ParseID(taskID); err != nil {
			continue
		}
		metadata, err := s.readMetadata(taskID)
		if err != nil || metadata.ClosedAt == 0 || !time.UnixMilli(metadata.ClosedAt).Before(before) {
			continue
		}
		if err := s.Remove(taskID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (s *OutputStore) readMetadata(taskID string) (OutputMetadata, error) {
	file, err := openSecureFile(s.dir, s.root, metadataName(taskID))
	if err != nil {
		return OutputMetadata{}, fmt.Errorf("opening task output metadata: %w", err)
	}
	defer file.Close()
	var metadata OutputMetadata
	if err := json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&metadata); err != nil {
		return OutputMetadata{}, fmt.Errorf("decoding task output metadata: %w", err)
	}
	if metadata.Version != 1 || metadata.TaskID != taskID {
		return OutputMetadata{}, fmt.Errorf("invalid task output metadata for %s", taskID)
	}
	return metadata, nil
}

func (o *Output) persistMetadataLocked() error {
	data, err := json.Marshal(o.metadata)
	if err != nil {
		return fmt.Errorf("encoding task output metadata: %w", err)
	}
	if err := o.meta.Truncate(0); err != nil {
		return fmt.Errorf("truncating task output metadata: %w", err)
	}
	if _, err := o.meta.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding task output metadata: %w", err)
	}
	if err := writeFull(o.meta, append(data, '\n')); err != nil {
		return fmt.Errorf("writing task output metadata: %w", err)
	}
	if err := o.meta.Sync(); err != nil {
		return fmt.Errorf("syncing task output metadata: %w", err)
	}
	return nil
}

func (o *Output) setStorageErrorLocked(err error) {
	if o.metadata.StorageError == "" {
		o.metadata.StorageError = err.Error()
		_ = o.persistMetadataLocked()
	}
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func outputName(taskID string, stream OutputStream) string {
	return taskID + "." + string(stream)
}

func metadataName(taskID string) string {
	return taskID + ".meta.json"
}

func outputNames(taskID string) []string {
	return []string{
		outputName(taskID, OutputStreamStdout),
		outputName(taskID, OutputStreamStderr),
		outputName(taskID, OutputStreamMerged),
		metadataName(taskID),
	}
}

func parseOutputRef(ref string) (string, error) {
	const prefix = "task-output:"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("invalid task output reference")
	}
	taskID := strings.TrimPrefix(ref, prefix)
	if _, err := ParseID(taskID); err != nil {
		return "", err
	}
	return taskID, nil
}

func WaitForOutput(ctx context.Context, done <-chan struct{}, wait bool, timeout time.Duration) (RetrievalStatus, error) {
	select {
	case <-done:
		return RetrievalReady, nil
	default:
	}
	if !wait {
		return RetrievalNotReady, nil
	}
	if timeout < 0 || timeout > MaxOutputWait {
		return "", fmt.Errorf("output wait timeout must be between 0 and %s", MaxOutputWait)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return RetrievalReady, nil
	case <-timer.C:
		return RetrievalTimeout, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
