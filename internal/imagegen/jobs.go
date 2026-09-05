package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/pubsub"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/google/uuid"
)

const (
	ModeGenerate = "generate"
	ModeEdit     = "edit"

	MaxConcurrentJobs = 4
	MaxQueuedJobs     = 50
	MaxInputImages    = 16

	maxInputImageBytes  = int64(50 << 20)
	maxInputTotalBytes  = int64(200 << 20)
	maxOutputImageBytes = int64(100 << 20)
	imageJobStopTimeout = 5 * time.Second
)

type JobRequest struct {
	Owner           *providerplugin.ImageOwner `json:"owner,omitempty"`
	OutputExtension string                     `json:"output_extension,omitempty"`
	Mode            string                     `json:"mode"`
	Backend         Backend                    `json:"backend,omitempty"`
	Prompt          string                     `json:"prompt"`
	Model           string                     `json:"model,omitempty"`
	Count           int                        `json:"count"`
	Quality         Quality                    `json:"quality,omitempty"`
	Size            string                     `json:"size,omitempty"`
	Background      Background                 `json:"background,omitempty"`
	InputPaths      []string                   `json:"input_paths,omitempty"`
	OutputPaths     []string                   `json:"output_paths"`
	Force           bool                       `json:"force,omitempty"`
}

type JobResult struct {
	Owner     *providerplugin.ImageOwner `json:"owner,omitempty"`
	Success   bool                       `json:"success"`
	Mode      string                     `json:"mode"`
	Requested int                        `json:"requested,omitempty"`
	Outputs   []string                   `json:"outputs"`
	Failures  []ImageVariantFailure      `json:"failures,omitempty"`
	AuthMode  string                     `json:"auth_mode,omitempty"`
	Model     string                     `json:"model,omitempty"`
	Error     string                     `json:"error,omitempty"`
}

func NumberedOutputName(backend Backend, index int) string {
	extension := ".png"
	if backend == BackendFlow {
		extension = ".jpg"
	}
	return fmt.Sprintf("image_%d%s", index, extension)
}

type JobManagerOptions struct {
	Setup         *SetupService
	PluginRuntime *PluginRuntime
	Executor      func(context.Context, JobRequest) (*Response, error)
	ClientFactory func() *Client
	MaxConcurrent int
	MaxQueued     int
}

type ImageJob struct {
	ID          string
	Description string
	Ownership   managedtask.Ownership
	Request     JobRequest

	mu            sync.Mutex
	state         managedtask.State
	createdAt     int64
	finalOutput   string
	cancel        context.CancelFunc
	done          chan struct{}
	executionDone chan struct{}
	doneOnce      sync.Once
	executionOnce sync.Once
	releaseOnce   sync.Once
	stopOnce      sync.Once
	notified      bool
	notification  *managedtask.Notification
	persist       func(*ImageJob) error
	notify        func(managedtask.Notification)
	release       func(*ImageJob)
}

type JobManager struct {
	setup                  *SetupService
	pluginRuntime          *PluginRuntime
	mu                     sync.RWMutex
	workspaceID            string
	jobs                   map[string]*ImageJob
	reservedPaths          map[string]string
	queue                  chan *ImageJob
	maxQueued              int
	active                 int
	running                int
	memoryReleaseMu        sync.Mutex
	memoryReleaseScheduled bool
	releaseMemory          func()
	execute                func(context.Context, JobRequest) (*Response, error)
	clientFactory          func() *Client
	recordStore            *managedtask.Store
	notifications          *pubsub.Broker[managedtask.Notification]
	ctx                    context.Context
	cancel                 context.CancelFunc
	closed                 bool
	workers                sync.WaitGroup
}

func NewJobManager(workspaceID string) *JobManager {
	manager, err := NewJobManagerWithStore(workspaceID, nil, JobManagerOptions{})
	if err != nil {
		panic(err)
	}
	return manager
}

func NewJobManagerWithStore(workspaceID string, recordStore *managedtask.Store, options JobManagerOptions) (*JobManager, error) {
	maxConcurrent := options.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = MaxConcurrentJobs
	}
	maxQueued := options.MaxQueued
	if maxQueued == 0 {
		maxQueued = MaxQueuedJobs
	}
	if maxConcurrent < 1 || maxConcurrent > MaxConcurrentJobs {
		return nil, fmt.Errorf("image job concurrency must be between 1 and %d", MaxConcurrentJobs)
	}
	if maxQueued < maxConcurrent {
		return nil, fmt.Errorf("image job queue capacity must be at least %d", maxConcurrent)
	}
	if options.Setup != nil && (options.PluginRuntime == nil || options.Setup.Runtime != options.PluginRuntime) {
		return nil, errors.New("image setup must use the job manager runtime")
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &JobManager{
		setup:         options.Setup,
		workspaceID:   workspaceID,
		pluginRuntime: options.PluginRuntime,
		jobs:          make(map[string]*ImageJob),
		reservedPaths: make(map[string]string),
		queue:         make(chan *ImageJob, maxQueued),
		maxQueued:     maxQueued,
		execute:       options.Executor,
		clientFactory: options.ClientFactory,
		recordStore:   recordStore,
		notifications: pubsub.NewBroker[managedtask.Notification](),
		releaseMemory: debug.FreeOSMemory,
		ctx:           ctx,
		cancel:        cancel,
	}
	if manager.clientFactory == nil {
		manager.clientFactory = NewClient
	}
	if manager.execute == nil {
		manager.execute = manager.executeRequest
	}
	if err := manager.recover(); err != nil {
		cancel()
		return nil, err
	}
	for range maxConcurrent {
		manager.workers.Go(manager.worker)
	}
	return manager, nil
}

func (m *JobManager) recover() error {
	if m.recordStore == nil {
		return nil
	}
	records, err := m.recordStore.List()
	if err != nil {
		return fmt.Errorf("loading background image records: %w", err)
	}
	for _, record := range records {
		if record.Type != managedtask.TypeImage || record.Ownership.WorkspaceID != m.workspaceID {
			continue
		}
		state := managedtask.StateFromRecord(record.State)
		request := jobRequestFromRecord(record.Image)
		finalOutput := record.Image.FinalOutput
		if !state.Status.Terminal() {
			state.Status = managedtask.StatusLost
			state.EndedAt = time.Now()
			state.LostReason = "image job was active when the workspace process restarted"
			finalOutput = encodeJobResult(JobResult{
				Success: false,
				Mode:    request.Mode,
				Owner:   request.Owner,
				Model:   request.Model,
				Error:   state.LostReason,
			})
		}
		done := make(chan struct{})
		close(done)
		executionDone := make(chan struct{})
		close(executionDone)
		job := &ImageJob{
			ID:            record.ID,
			Description:   record.Description,
			Ownership:     record.Ownership,
			Request:       request,
			state:         state,
			createdAt:     record.CreatedAt,
			finalOutput:   finalOutput,
			done:          done,
			executionDone: executionDone,
			notified:      record.Image.NotificationEmitted,
			notification:  record.Notification,
		}
		m.configureJob(job)
		if job.notification == nil {
			job.notification = job.newNotificationLocked()
			job.notified = false
		}
		if err := m.recordStore.Put(job.record()); err != nil {
			return fmt.Errorf("recovering background image job %s: %w", record.ID, err)
		}
		m.jobs[job.ID] = job
	}
	return nil
}

func jobRequestFromRecord(record *managedtask.ImageRecord) JobRequest {
	var owner *providerplugin.ImageOwner
	if record.PluginID != "" || record.PluginVersion != "" || record.PluginDigest != "" {
		owner = &providerplugin.ImageOwner{Backend: record.Backend, PluginID: record.PluginID, Version: record.PluginVersion, Digest: record.PluginDigest}
	}
	return JobRequest{
		Owner:           owner,
		OutputExtension: record.OutputExtension,
		Mode:            record.Mode,
		Backend:         Backend(record.Backend),
		Prompt:          record.Prompt,
		Model:           record.Model,
		Count:           record.Count,
		Quality:         Quality(record.Quality),
		Size:            record.Size,
		Background:      Background(record.Background),
		InputPaths:      append([]string(nil), record.InputPaths...),
		OutputPaths:     append([]string(nil), record.OutputPaths...),
		Force:           record.Force,
	}
}

func (m *JobManager) configureJob(job *ImageJob) {
	job.persist = m.persistJob
	job.notify = func(notification managedtask.Notification) {
		m.notifications.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, notification)
	}
	job.release = m.releaseJob
}

func (m *JobManager) persistJob(job *ImageJob) error {
	if m.recordStore == nil {
		return nil
	}
	return m.recordStore.Put(job.record())
}

func (j *ImageJob) record() managedtask.Record {
	var owner providerplugin.ImageOwner
	if j.Request.Owner != nil {
		owner = *j.Request.Owner
	}
	return managedtask.Record{
		ID:           j.ID,
		Type:         managedtask.TypeImage,
		Description:  j.Description,
		Ownership:    j.Ownership,
		CreatedAt:    j.createdAt,
		State:        managedtask.StateToRecord(j.state),
		OutputRef:    j.outputRef(),
		Notification: j.notification,
		Image: &managedtask.ImageRecord{
			PluginID:            owner.PluginID,
			PluginVersion:       owner.Version,
			PluginDigest:        owner.Digest,
			OutputExtension:     j.Request.OutputExtension,
			Mode:                j.Request.Mode,
			Backend:             string(j.Request.Backend),
			Prompt:              j.Request.Prompt,
			Model:               j.Request.Model,
			Count:               j.Request.Count,
			Quality:             string(j.Request.Quality),
			Size:                j.Request.Size,
			Background:          string(j.Request.Background),
			InputPaths:          append([]string(nil), j.Request.InputPaths...),
			OutputPaths:         append([]string(nil), j.Request.OutputPaths...),
			Force:               j.Request.Force,
			FinalOutput:         j.finalOutput,
			NotificationEmitted: j.notified,
		},
	}
}

func (j *ImageJob) outputRef() string {
	if j.state.Status == managedtask.StatusCompleted && j.finalOutput != "" {
		var result JobResult
		if json.Unmarshal([]byte(j.finalOutput), &result) == nil && len(result.Outputs) > 0 {
			return "file:" + result.Outputs[0]
		}
	}
	if len(j.Request.OutputPaths) == 0 {
		return ""
	}
	return "file:" + j.Request.OutputPaths[0]
}

func (m *JobManager) Enqueue(request JobRequest, description string, ownership managedtask.Ownership) (managedtask.View, error) {
	view, _, err := m.enqueue(request, "", description, ownership)
	return view, err
}

func (m *JobManager) EnqueueNumbered(request JobRequest, outputDirectory, description string, ownership managedtask.Ownership) (managedtask.View, []string, error) {
	if outputDirectory == "" {
		return managedtask.View{}, nil, errors.New("numbered output directory is required")
	}
	return m.enqueue(request, outputDirectory, description, ownership)
}

func (m *JobManager) enqueue(request JobRequest, outputDirectory, description string, ownership managedtask.Ownership) (managedtask.View, []string, error) {
	if ownership.ParentSessionID == "" {
		return managedtask.View{}, nil, errors.New("parent session ID is required for a background image job")
	}
	var err error
	request, err = m.PrepareRequest(m.ctx, request)
	if err != nil {
		return managedtask.View{}, nil, err
	}
	if err := validateJobRequestBeforeAllocation(request, outputDirectory != ""); err != nil {
		return managedtask.View{}, nil, err
	}
	if outputDirectory == "" {
		if err := preflightJobPaths(request); err != nil {
			return managedtask.View{}, nil, err
		}
	} else {
		if !filepath.IsAbs(outputDirectory) {
			return managedtask.View{}, nil, fmt.Errorf("numbered output directory must be absolute: %s", outputDirectory)
		}
		if err := preflightJobInputs(request); err != nil {
			return managedtask.View{}, nil, err
		}
		if err := os.MkdirAll(outputDirectory, 0o755); err != nil {
			return managedtask.View{}, nil, fmt.Errorf("create output directory %q: %w", outputDirectory, err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return managedtask.View{}, nil, errors.New("background image job manager is closed")
	}
	if m.active >= m.maxQueued {
		return managedtask.View{}, nil, fmt.Errorf("background image queue capacity reached: %d active or pending jobs", m.maxQueued)
	}
	if outputDirectory != "" {
		paths, err := nextNumberedOutputPaths(outputDirectory, request.Count, request.Force, request.Backend, m.reservedPaths, request.OutputExtension)
		if err != nil {
			return managedtask.View{}, nil, err
		}
		request.OutputPaths = paths
	}
	if err := validateJobRequest(request); err != nil {
		return managedtask.View{}, nil, err
	}
	for _, path := range request.OutputPaths {
		if existingID, exists := m.reservedPaths[path]; exists {
			return managedtask.View{}, nil, fmt.Errorf("output path is already reserved by image job %s: %s", existingID, path)
		}
	}
	id, err := managedtask.NewID(managedtask.TypeImage)
	if err != nil {
		return managedtask.View{}, nil, err
	}
	ownership.WorkspaceID = m.workspaceID
	job := &ImageJob{
		ID:            id,
		Description:   description,
		Ownership:     ownership,
		Request:       request,
		state:         managedtask.State{Status: managedtask.StatusPending},
		createdAt:     time.Now().UnixMilli(),
		done:          make(chan struct{}),
		executionDone: make(chan struct{}),
	}
	m.configureJob(job)
	if err := job.persist(job); err != nil {
		return managedtask.View{}, nil, fmt.Errorf("persisting background image job: %w", err)
	}
	m.jobs[id] = job
	for _, path := range request.OutputPaths {
		m.reservedPaths[path] = id
	}
	m.active++
	m.queue <- job
	return job.Info(), append([]string(nil), request.OutputPaths...), nil
}

func validateJobRequest(request JobRequest) error {
	return validateJobRequestBeforeAllocation(request, false)
}

func validateJobRequestBeforeAllocation(request JobRequest, numbered bool) error {
	if request.Mode != ModeGenerate && request.Mode != ModeEdit {
		return fmt.Errorf("image mode must be %q or %q", ModeGenerate, ModeEdit)
	}
	if request.Count < minImageCount || request.Count > maxImageCount {
		return fmt.Errorf("image count must be between %d and %d", minImageCount, maxImageCount)
	}
	if !numbered && len(request.OutputPaths) != request.Count {
		return fmt.Errorf("image job has %d output paths for %d requested images", len(request.OutputPaths), request.Count)
	}
	if request.Owner == nil {
		if err := ValidateGenerateRequest(GenerateRequest{
			Backend:    request.Backend,
			Prompt:     request.Prompt,
			Model:      request.Model,
			N:          request.Count,
			Quality:    request.Quality,
			Size:       request.Size,
			Background: request.Background,
		}); err != nil {
			return err
		}
	}
	if request.Mode == ModeGenerate && len(request.InputPaths) != 0 {
		return errors.New("input images are only valid in edit mode")
	}
	if request.Mode == ModeEdit && len(request.InputPaths) == 0 {
		return errors.New("at least one input image is required in edit mode")
	}
	if len(request.InputPaths) > MaxInputImages {
		return fmt.Errorf("at most %d input images are supported", MaxInputImages)
	}
	seen := make(map[string]struct{}, len(request.OutputPaths))
	for _, path := range request.OutputPaths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("output path must be absolute: %s", path)
		}
		if _, exists := seen[path]; exists {
			return fmt.Errorf("duplicate output path: %s", path)
		}
		seen[path] = struct{}{}
	}
	for _, path := range request.InputPaths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("input path must be absolute: %s", path)
		}
	}
	return nil
}

func preflightJobPaths(request JobRequest) error {
	if err := preflightJobInputs(request); err != nil {
		return err
	}
	return preflightJobOutputs(request)
}

func preflightJobInputs(request JobRequest) error {
	for _, path := range request.InputPaths {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect input image %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("input image is not a regular file: %s", path)
		}
		if info.Size() > maxInputImageBytes {
			return fmt.Errorf("input image %q exceeds %d bytes", path, maxInputImageBytes)
		}
	}
	return nil
}

func preflightJobOutputs(request JobRequest) error {
	for _, path := range request.OutputPaths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create output directory %q: %w", filepath.Dir(path), err)
		}
		info, err := os.Stat(path)
		switch {
		case err == nil && info.IsDir():
			return fmt.Errorf("output path %q is a directory", path)
		case err == nil && !request.Force:
			return fmt.Errorf("output already exists: %s", path)
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("inspect output path %q: %w", path, err)
		}
	}
	return nil
}

func nextNumberedOutputPaths(directory string, count int, force bool, backend Backend, reserved map[string]string, extensions ...string) ([]string, error) {
	paths := make([]string, 0, count)
	for index := 1; len(paths) < count; index++ {
		name := NumberedOutputName(backend, index)
		if len(extensions) > 0 && extensions[0] != "" {
			name = fmt.Sprintf("image_%d%s", index, extensions[0])
		}
		path := filepath.Join(directory, name)
		if _, exists := reserved[path]; exists {
			continue
		}
		info, err := os.Lstat(path)
		switch {
		case err == nil && force && info.Mode().IsRegular():
			paths = append(paths, path)
		case err == nil:
			continue
		case errors.Is(err, os.ErrNotExist):
			paths = append(paths, path)
		default:
			return nil, fmt.Errorf("inspect output path %q: %w", path, err)
		}
	}
	return paths, nil
}

func (m *JobManager) worker() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case job := <-m.queue:
			if job != nil {
				m.run(job)
			}
		}
	}
}

func (m *JobManager) run(job *ImageJob) {
	job.mu.Lock()
	if job.state.Status.Terminal() {
		job.mu.Unlock()
		job.executionOnce.Do(func() { close(job.executionDone) })
		return
	}
	runCtx, cancel := context.WithCancel(m.ctx)
	job.cancel = cancel
	job.state.Status = managedtask.StatusRunning
	job.state.StartedAt = time.Now()
	persistErr := job.persist(job)
	job.mu.Unlock()

	m.beginExecution()
	defer func() {
		m.endExecution()
		job.executionOnce.Do(func() { close(job.executionDone) })
	}()
	m.executeJob(runCtx, job, persistErr)
}

func (m *JobManager) beginExecution() {
	m.memoryReleaseMu.Lock()
	m.mu.Lock()
	m.running++
	m.mu.Unlock()
	m.memoryReleaseMu.Unlock()
}

func (m *JobManager) executeJob(ctx context.Context, job *ImageJob, persistErr error) {
	if persistErr != nil {
		m.finish(job, nil, nil, fmt.Errorf("persisting background image job start: %w", persistErr))
		return
	}
	response, err := m.execute(ctx, job.Request)
	defer clearImageResponseData(response)
	var outputs []string
	if err == nil {
		outputs, err = writeJobImages(ctx, job.Request, response)
	}
	m.finish(job, response, outputs, err)
}

func (m *JobManager) endExecution() {
	m.mu.Lock()
	if m.running > 0 {
		m.running--
	}
	release := m.scheduleMemoryReleaseLocked()
	m.mu.Unlock()
	if release {
		go m.releaseMemoryWhenIdle()
	}
}

func (m *JobManager) executeRequest(ctx context.Context, request JobRequest) (*Response, error) {
	if request.Owner != nil {
		if m.pluginRuntime == nil {
			return nil, errors.New("image plugin runtime is unavailable")
		}
		var images []EditImage
		var err error
		if request.Mode == ModeEdit {
			images, err = loadEditImages(ctx, request.InputPaths)
			if err != nil {
				return nil, err
			}
		}
		return m.pluginRuntime.Execute(ctx, *request.Owner, request, images)
	}
	client := m.clientFactory()
	if client == nil {
		return nil, errors.New("image client is unavailable")
	}
	if request.Mode == ModeGenerate {
		return client.Generate(ctx, GenerateRequest{
			Backend:    request.Backend,
			Prompt:     request.Prompt,
			Model:      request.Model,
			N:          request.Count,
			Quality:    request.Quality,
			Size:       request.Size,
			Background: request.Background,
		})
	}
	images, err := loadEditImages(ctx, request.InputPaths)
	if err != nil {
		return nil, err
	}
	return client.Edit(ctx, EditRequest{
		Backend:    request.Backend,
		Images:     images,
		Prompt:     request.Prompt,
		Model:      request.Model,
		N:          request.Count,
		Quality:    request.Quality,
		Size:       request.Size,
		Background: request.Background,
	})
}

func loadEditImages(ctx context.Context, paths []string) ([]EditImage, error) {
	images := make([]EditImage, 0, len(paths))
	var total int64
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open input image %q: %w", path, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxInputImageBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read input image %q: %w", path, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close input image %q: %w", path, closeErr)
		}
		if int64(len(data)) > maxInputImageBytes {
			return nil, fmt.Errorf("input image %q exceeds %d bytes", path, maxInputImageBytes)
		}
		total += int64(len(data))
		if total > maxInputTotalBytes {
			return nil, fmt.Errorf("input images exceed %d total bytes", maxInputTotalBytes)
		}
		mimeType := http.DetectContentType(data)
		if !strings.HasPrefix(mimeType, "image/") {
			mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
		}
		if !strings.HasPrefix(mimeType, "image/") {
			return nil, fmt.Errorf("input file is not a supported image: %s", path)
		}
		images = append(images, EditImage{Filename: filepath.Base(path), MIMEType: mimeType, Data: data})
	}
	return images, nil
}

func writeJobImages(ctx context.Context, request JobRequest, response *Response) ([]string, error) {
	response, err := finalizeImageResponse(response, len(request.OutputPaths))
	if err != nil {
		return nil, err
	}
	defer clearImageResponseData(response)
	type stagedImage struct {
		variant   int
		path      string
		temporary string
	}
	staged := make([]stagedImage, 0, len(response.Data))
	defer func() {
		for _, image := range staged {
			_ = os.Remove(image.temporary)
		}
	}()
	failures := append([]ImageVariantFailure(nil), response.Failures...)
	for index := range response.Data {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		variant := response.Data[index].Variant
		encoded := response.Data[index].B64JSON
		response.Data[index].B64JSON = ""
		path := request.OutputPaths[variant-1]
		temporary, stageErr := stageJobImage(ctx, path, encoded)
		if stageErr != nil {
			failures = append(failures, ImageVariantFailure{Variant: variant, Error: fmt.Sprintf("decode image data: %v", stageErr)})
			continue
		}
		staged = append(staged, stagedImage{variant: variant, path: path, temporary: temporary})
	}

	outputs := make([]string, 0, len(staged))
	created := make([]string, 0, len(staged))
	for _, image := range staged {
		if err := ctx.Err(); err != nil {
			removeCreatedOutputs(created)
			return nil, err
		}
		if writeErr := writeStagedImage(ctx, image.temporary, image.path, request.Force); writeErr != nil {
			if err := ctx.Err(); err != nil {
				removeCreatedOutputs(created)
				return nil, err
			}
			failures = append(failures, ImageVariantFailure{Variant: image.variant, Error: writeErr.Error()})
			continue
		}
		outputs = append(outputs, image.path)
		if !request.Force {
			created = append(created, image.path)
		}
	}
	sort.SliceStable(failures, func(left, right int) bool {
		return failures[left].Variant < failures[right].Variant
	})
	response.Failures = failures
	if len(outputs) == 0 {
		return nil, imageVariantFailuresError(failures)
	}
	return outputs, nil
}

func stageJobImage(ctx context.Context, outputPath, encoded string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(outputPath), ".crux-image-*")
	if err != nil {
		return "", fmt.Errorf("create temporary image file: %w", err)
	}
	temporary := file.Name()
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o644); err != nil {
		return "", fmt.Errorf("set temporary image permissions: %w", err)
	}
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
	written, writeErr := io.Copy(file, io.LimitReader(&jobContextReader{ctx: ctx, reader: decoder}, maxOutputImageBytes+1))
	closeErr := file.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if written > maxOutputImageBytes {
		return "", fmt.Errorf("decoded image exceeds %d bytes", maxOutputImageBytes)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close temporary image file: %w", closeErr)
	}
	remove = false
	return temporary, nil
}

func writeStagedImage(ctx context.Context, temporary, path string, force bool) error {
	input, err := os.Open(temporary)
	if err != nil {
		return fmt.Errorf("open staged image for %q: %w", path, err)
	}
	defer input.Close()
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	output, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("output already exists: %s", path)
		}
		return fmt.Errorf("create image %q: %w", path, err)
	}
	_, writeErr := io.Copy(output, &jobContextReader{ctx: ctx, reader: input})
	closeErr := output.Close()
	if writeErr != nil || closeErr != nil {
		if !force {
			_ = os.Remove(path)
		}
		if writeErr != nil {
			return fmt.Errorf("write image %q: %w", path, writeErr)
		}
		return fmt.Errorf("close image %q: %w", path, closeErr)
	}
	return nil
}

type jobContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *jobContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func clearImageResponseData(response *Response) {
	if response == nil {
		return
	}
	for index := range response.Data {
		response.Data[index].B64JSON = ""
	}
}

func removeCreatedOutputs(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func (m *JobManager) finish(job *ImageJob, response *Response, outputs []string, runErr error) {
	job.mu.Lock()
	if job.state.Status.Terminal() {
		job.mu.Unlock()
		return
	}
	now := time.Now()
	result := JobResult{
		Owner:     job.Request.Owner,
		Success:   runErr == nil && len(outputs) > 0,
		Mode:      job.Request.Mode,
		Requested: job.Request.Count,
		Outputs:   append([]string(nil), outputs...),
		Model:     job.Request.Model,
	}
	if response != nil {
		if job.Request.Owner == nil {
			result.AuthMode = authModeName(response.AuthMode)
		}
		result.Model = response.Model
		result.Failures = append([]ImageVariantFailure(nil), response.Failures...)
	}
	job.state.EndedAt = now
	switch {
	case !job.state.StopRequestedAt.IsZero():
		job.state.Status = managedtask.StatusKilled
		job.state.Interrupted = true
		result.Success = false
		result.Error = "image job was canceled"
	case runErr != nil:
		job.state.Status = managedtask.StatusFailed
		job.state.ErrorCode = "image_generation_failed"
		job.state.ErrorMessage = runErr.Error()
		result.Error = runErr.Error()
	default:
		job.state.Status = managedtask.StatusCompleted
	}
	job.finalOutput = encodeJobResult(result)
	notification := job.notificationLocked()
	_ = job.persist(job)
	job.mu.Unlock()
	job.releaseOnce.Do(func() { job.release(job) })
	job.doneOnce.Do(func() { close(job.done) })
	if notification != nil {
		job.notify(*notification)
	}
}

func authModeName(mode AuthMode) string {
	switch mode {
	case AuthAPIKey:
		return "openai_api_key"
	case AuthFlow:
		return "flow"
	default:
		return "codex"
	}
}

func encodeJobResult(result JobResult) string {
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf(`{"success":false,"error":%q}`, err.Error())
	}
	return string(data)
}

func (j *ImageJob) notificationLocked() *managedtask.Notification {
	if j.notified || !j.state.Status.Terminal() {
		return nil
	}
	j.notified = true
	j.notification = j.newNotificationLocked()
	return j.notification
}

func (j *ImageJob) summaryLocked() string {
	operation := "Image generation"
	if j.Request.Mode == ModeEdit {
		operation = "Image edit"
	}
	switch j.state.Status {
	case managedtask.StatusCompleted:
		var result JobResult
		if json.Unmarshal([]byte(j.finalOutput), &result) == nil && len(result.Failures) > 0 {
			return fmt.Sprintf("%s completed with %d of %d variants", operation, len(result.Outputs), result.Requested)
		}
		if len(j.Request.OutputPaths) > 1 {
			return fmt.Sprintf("%s completed with %d variants", operation, len(j.Request.OutputPaths))
		}
		return operation + " completed"
	case managedtask.StatusFailed:
		return operation + " failed"
	case managedtask.StatusKilled:
		return operation + " canceled"
	case managedtask.StatusLost:
		return operation + " interrupted"
	default:
		return operation + " updated"
	}
}

func (j *ImageJob) newNotificationLocked() *managedtask.Notification {
	return &managedtask.Notification{
		ID:              uuid.NewString(),
		TaskID:          j.ID,
		TaskType:        managedtask.TypeImage,
		ToolUseID:       j.Ownership.OriginToolCallID,
		WorkspaceID:     j.Ownership.WorkspaceID,
		ParentSessionID: j.Ownership.ParentSessionID,
		Status:          j.state.Status,
		Summary:         j.summaryLocked(),
		EndedAt:         j.state.EndedAt,
		OutputRef:       j.outputRef(),
		Interrupted:     j.state.Interrupted,
		ErrorCode:       j.state.ErrorCode,
		ErrorMessage:    j.state.ErrorMessage,
		LostReason:      j.state.LostReason,
		FinalOutput:     j.finalOutput,
	}
}

func (m *JobManager) releaseJob(job *ImageJob) {
	m.mu.Lock()
	for _, path := range job.Request.OutputPaths {
		if m.reservedPaths[path] == job.ID {
			delete(m.reservedPaths, path)
		}
	}
	if m.active > 0 {
		m.active--
	}
	release := m.scheduleMemoryReleaseLocked()
	m.mu.Unlock()
	if release {
		go m.releaseMemoryWhenIdle()
	}
}

func (m *JobManager) scheduleMemoryReleaseLocked() bool {
	if m.active != 0 || m.running != 0 || m.memoryReleaseScheduled {
		return false
	}
	m.memoryReleaseScheduled = true
	return true
}

func (m *JobManager) releaseMemoryWhenIdle() {
	m.memoryReleaseMu.Lock()
	defer m.memoryReleaseMu.Unlock()

	m.mu.Lock()
	if !m.memoryReleaseScheduled {
		m.mu.Unlock()
		return
	}
	if m.active != 0 || m.running != 0 {
		m.memoryReleaseScheduled = false
		m.mu.Unlock()
		return
	}
	releaseMemory := m.releaseMemory
	m.mu.Unlock()
	if releaseMemory != nil {
		releaseMemory()
	}
	m.mu.Lock()
	m.memoryReleaseScheduled = false
	m.mu.Unlock()
}

func (m *JobManager) Get(id string) (*ImageJob, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	return job, ok
}

func (m *JobManager) List() []managedtask.View {
	m.mu.RLock()
	jobs := make([]*ImageJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.RUnlock()
	views := make([]managedtask.View, 0, len(jobs))
	for _, job := range jobs {
		views = append(views, job.Info())
	}
	sort.SliceStable(views, func(left, right int) bool {
		return views[left].ID < views[right].ID
	})
	return views
}

func (j *ImageJob) Info() managedtask.View {
	j.mu.Lock()
	defer j.mu.Unlock()
	return managedtask.View{
		ID:          j.ID,
		Type:        managedtask.TypeImage,
		Description: j.Description,
		Ownership:   j.Ownership,
		State:       j.state,
		OutputRef:   j.outputRef(),
		FinalOutput: j.finalOutput,
	}
}

func (m *JobManager) Output(ctx context.Context, id string, wait bool, timeout time.Duration) (managedtask.OutputResult, error) {
	job, err := m.job(id)
	if err != nil {
		return managedtask.OutputResult{}, err
	}
	status, err := managedtask.WaitForOutput(ctx, job.done, wait, timeout)
	info := job.Info()
	return managedtask.OutputResult{
		Task:            info,
		Output:          info.FinalOutput,
		RetrievalStatus: status,
		Status:          status,
		NextOffset:      int64(len(info.FinalOutput)),
	}, err
}

func (m *JobManager) Stop(ctx context.Context, id string) (managedtask.View, error) {
	job, err := m.job(id)
	if err != nil {
		return managedtask.View{}, err
	}
	job.requestStop()
	select {
	case <-job.done:
		return job.Info(), nil
	case <-ctx.Done():
		return job.Info(), ctx.Err()
	}
}

func (m *JobManager) job(id string) (*ImageJob, error) {
	taskType, err := managedtask.ParseID(id)
	if err != nil {
		return nil, err
	}
	if taskType != managedtask.TypeImage {
		return nil, fmt.Errorf("task %s is not a background image job", id)
	}
	job, ok := m.Get(id)
	if !ok {
		return nil, fmt.Errorf("background image job not found: %s", id)
	}
	return job, nil
}

func (j *ImageJob) requestStop() {
	j.stopOnce.Do(func() {
		j.mu.Lock()
		if j.state.Status.Terminal() {
			j.mu.Unlock()
			return
		}
		j.state.StopRequestedAt = time.Now()
		cancel := j.cancel
		if j.state.Status == managedtask.StatusPending {
			j.state.Status = managedtask.StatusKilled
			j.state.EndedAt = time.Now()
			j.state.Interrupted = true
			j.finalOutput = encodeJobResult(JobResult{
				Success: false,
				Mode:    j.Request.Mode,
				Owner:   j.Request.Owner,
				Model:   j.Request.Model,
				Error:   "image job was canceled before execution",
			})
			notification := j.notificationLocked()
			_ = j.persist(j)
			j.mu.Unlock()
			j.releaseOnce.Do(func() { j.release(j) })
			j.doneOnce.Do(func() { close(j.done) })
			j.executionOnce.Do(func() { close(j.executionDone) })
			if notification != nil {
				j.notify(*notification)
			}
			return
		}
		_ = j.persist(j)
		j.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
}

func (j *ImageJob) markLost(reason string) {
	j.mu.Lock()
	if j.state.Status.Terminal() {
		j.mu.Unlock()
		return
	}
	j.state.Status = managedtask.StatusLost
	j.state.EndedAt = time.Now()
	j.state.LostReason = reason
	j.finalOutput = encodeJobResult(JobResult{
		Success: false,
		Mode:    j.Request.Mode,
		Owner:   j.Request.Owner,
		Model:   j.Request.Model,
		Error:   reason,
	})
	notification := j.notificationLocked()
	_ = j.persist(j)
	j.mu.Unlock()
	j.releaseOnce.Do(func() { j.release(j) })
	j.doneOnce.Do(func() { close(j.done) })
	if notification != nil {
		j.notify(*notification)
	}
}

func (m *JobManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

func (m *JobManager) RunningCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}

func (m *JobManager) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[managedtask.Notification] {
	return m.notifications.Subscribe(ctx)
}

func (m *JobManager) StopAll(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	jobs := make([]*ImageJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()

	for _, job := range jobs {
		job.requestStop()
	}
	m.cancel()
	for _, job := range jobs {
		select {
		case <-job.done:
		case <-ctx.Done():
			job.markLost("workspace shutdown deadline expired before image generation terminated")
		}
	}
	m.notifications.Shutdown()
}
