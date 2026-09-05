package codebaseindex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type StoreState string

const (
	StoreStateDisabled StoreState = "disabled"
	StoreStateMissing  StoreState = "missing"
	StoreStateIndexing StoreState = "indexing"
	StoreStateReady    StoreState = "ready"
	StoreStateStale    StoreState = "stale"
	StoreStateFailed   StoreState = "failed"
)

type IndexProgress struct {
	Stage          string
	FilesTotal     int
	FilesProcessed int
	ChunksCreated  int
	FilesSkipped   int
	CurrentPath    string
}

type StoreStatus struct {
	State            StoreState
	Serving          bool
	ProjectRoot      string
	DatabasePath     string
	StoreDirectory   string
	SourceMode       string
	CredentialStatus string
	Model            string
	FilesTotal       int
	FilesProcessed   int
	ChunksCreated    int
	FilesSkipped     int
	CurrentPath      string
	Stage            string
	StartedAt        time.Time
	FinishedAt       time.Time
	Err              error
}

type ProjectIndexOptions struct {
	ProjectRoot            string
	ConfiguredDatabasePath string
	StoreDirectory         string
	Enabled                bool
	Filters                ProjectFilters
}

type StoreUnavailableError struct {
	ProjectRoot string
	State       StoreState
	Err         error
}

func (e *StoreUnavailableError) Error() string {
	switch e.State {
	case StoreStateDisabled:
		return "semantic code indexing and search are disabled"
	case StoreStateIndexing:
		return "semantic code index is being built in the background"
	case StoreStateStale:
		return "semantic code index is being refreshed in the background"
	case StoreStateFailed:
		if e.Err != nil {
			return fmt.Sprintf("semantic code index background build failed: %v", e.Err)
		}
		return "semantic code index background build failed"
	default:
		return "semantic code index is not available yet"
	}
}

func (e *StoreUnavailableError) Unwrap() error {
	return e.Err
}

type backgroundIndexJob struct {
	status StoreStatus
	digest string
	id     uint64
	cancel context.CancelFunc
}

var backgroundIndexes = struct {
	sync.RWMutex
	jobs   map[string]backgroundIndexJob
	nextID uint64
}{jobs: make(map[string]backgroundIndexJob)}

var (
	findImportDatabase       = FindImportDatabasePath
	runNativeProjectIndexing = func(ctx context.Context, projectRoot, storeDirectory string, filters ProjectFilters, report func(IndexProgress)) error {
		client := NewGitHubClient(http.DefaultClient, CodebaseIndexToken, GitHubSemanticUserAgent)
		return buildNativeProjectStore(ctx, projectRoot, storeDirectory, filters, client, report)
	}
	runProjectIndexing = indexProjectWithFilters
)

func OpenReadyProject(projectRoot, storeDirectory string) (*Reader, error) {
	return OpenReadyProjectWithFilters(projectRoot, storeDirectory, ProjectFilters{})
}

func OpenReadyProjectWithFilters(projectRoot, storeDirectory string, filters ProjectFilters) (*Reader, error) {
	directory, err := resolveStoreDirectory(storeDirectory)
	if err != nil {
		return nil, err
	}
	filters = NormalizeProjectFilters(filters)
	filter := filterDigest(filters)
	options := ProjectIndexOptions{
		ProjectRoot:    projectRoot,
		StoreDirectory: directory,
		Enabled:        true,
		Filters:        filters,
	}
	var lastErr error
	for range 3 {
		catalog, loadErr := loadProjectCatalog(directory, projectRoot)
		if loadErr != nil {
			lastErr = loadErr
			continue
		}
		if catalog.Source.FilterDigest != filter {
			status := inspectProjectIndexStatus(options, false)
			return nil, &StoreUnavailableError{ProjectRoot: projectRoot, State: status.State, Err: status.Err}
		}
		releaseLease, leaseErr := acquireGenerationLease(context.Background(), catalog.Directory)
		if leaseErr != nil {
			return nil, leaseErr
		}
		leasedCatalog, loadErr := loadStoreCatalog(filepath.Join(catalog.Directory, "migration.json"))
		if loadErr == nil && leasedCatalog.Complete && leasedCatalog.ProjectRoot == projectRoot && leasedCatalog.Source.FilterDigest == filter && filepath.Clean(leasedCatalog.Directory) == filepath.Clean(catalog.Directory) {
			return &Reader{
				path:         leasedCatalog.Source.DatabasePath,
				annDirectory: directory,
				catalog:      &leasedCatalog,
				filters:      filters,
				filterDigest: filter,
				releaseLease: releaseLease,
			}, nil
		}
		releaseLease()
		if loadErr == nil {
			loadErr = fmt.Errorf("leased codebase index generation does not match active catalog")
		}
		lastErr = loadErr
	}
	status := inspectProjectIndexStatus(options, false)
	if status.Serving && lastErr != nil {
		return nil, lastErr
	}
	return nil, &StoreUnavailableError{ProjectRoot: projectRoot, State: status.State, Err: status.Err}
}

func StartProjectIndexing(ctx context.Context, projectRoot, configuredDatabasePath, storeDirectory string) StoreStatus {
	return ReconcileProjectIndexing(ctx, ProjectIndexOptions{
		ProjectRoot:            projectRoot,
		ConfiguredDatabasePath: configuredDatabasePath,
		StoreDirectory:         storeDirectory,
		Enabled:                true,
	})
}

func StartProjectIndexingWithFilters(ctx context.Context, projectRoot, configuredDatabasePath, storeDirectory string, filters ProjectFilters) StoreStatus {
	return ReconcileProjectIndexing(ctx, ProjectIndexOptions{
		ProjectRoot:            projectRoot,
		ConfiguredDatabasePath: configuredDatabasePath,
		StoreDirectory:         storeDirectory,
		Enabled:                true,
		Filters:                filters,
	})
}

func ReconcileProjectIndexing(ctx context.Context, options ProjectIndexOptions) StoreStatus {
	directory, err := resolveStoreDirectory(options.StoreDirectory)
	if err != nil {
		return statusWithDetails(options, options.StoreDirectory, StoreStatus{State: StoreStateFailed, FinishedAt: time.Now(), Err: err})
	}
	options.StoreDirectory = directory
	options.Filters = NormalizeProjectFilters(options.Filters)
	runStoreMaintenance(directory)
	digest := projectIndexOptionsDigest(options)
	key := projectIndexKey(options.ProjectRoot, directory)

	if !options.Enabled {
		backgroundIndexes.Lock()
		current := backgroundIndexes.jobs[key]
		if current.cancel != nil {
			current.cancel()
		}
		backgroundIndexes.nextID++
		status := statusWithDetails(options, directory, StoreStatus{State: StoreStateDisabled, FinishedAt: time.Now()})
		backgroundIndexes.jobs[key] = backgroundIndexJob{status: status, digest: digest, id: backgroundIndexes.nextID}
		backgroundIndexes.Unlock()
		return status
	}

	current := InspectProjectIndexStatus(options)
	if current.State == StoreStateReady || current.State == StoreStateIndexing {
		return current
	}

	backgroundIndexes.Lock()
	if existing := backgroundIndexes.jobs[key]; existing.cancel != nil {
		existing.cancel()
	}
	backgroundIndexes.nextID++
	jobID := backgroundIndexes.nextID
	status := current
	status = statusWithDetails(options, directory, status)
	status.State = StoreStateIndexing
	status.FinishedAt = time.Time{}
	status.Err = nil
	if status.StartedAt.IsZero() {
		status.StartedAt = time.Now()
	}
	if status.FilesProcessed > 0 || status.ChunksCreated > 0 || status.FilesSkipped > 0 {
		status.Stage = "Resuming index"
	} else {
		status.Stage = "Preparing index"
	}
	workerContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
	backgroundIndexes.jobs[key] = backgroundIndexJob{
		status: status,
		digest: digest,
		id:     jobID,
		cancel: cancel,
	}
	backgroundIndexes.Unlock()

	go func() {
		report := func(progress IndexProgress) {
			backgroundIndexes.Lock()
			current := backgroundIndexes.jobs[key]
			if current.id == jobID {
				current.status.Stage = progress.Stage
				current.status.FilesTotal = max(current.status.FilesTotal, progress.FilesTotal)
				current.status.FilesProcessed = max(current.status.FilesProcessed, progress.FilesProcessed)
				current.status.ChunksCreated = max(current.status.ChunksCreated, progress.ChunksCreated)
				current.status.FilesSkipped = max(current.status.FilesSkipped, progress.FilesSkipped)
				current.status.CurrentPath = progress.CurrentPath
				backgroundIndexes.jobs[key] = current
			}
			backgroundIndexes.Unlock()
		}
		err := runProjectIndexing(workerContext, options.ProjectRoot, options.ConfiguredDatabasePath, directory, options.Filters, report)
		if err == nil {
			if _, pruneErr := pruneStoreGenerations(directory); pruneErr != nil {
				slog.Warn("Could not prune codebase index store after indexing", "store_directory", directory, "error", pruneErr)
			}
		}
		backgroundIndexes.RLock()
		finished := backgroundIndexes.jobs[key].status
		backgroundIndexes.RUnlock()
		finished = statusWithDetails(options, directory, finished)
		finished.State = StoreStateReady
		finished.FinishedAt = time.Now()
		finished.CurrentPath = ""
		finished.Stage = "Complete"
		finished.Err = err
		if err == nil {
			if catalog, catalogErr := loadProjectCatalog(directory, options.ProjectRoot); catalogErr == nil {
				finished = statusWithCatalog(options, directory, catalog, finished)
				finished.State = StoreStateReady
				finished.FinishedAt = catalog.IndexedAt
				finished.CurrentPath = ""
				finished.Stage = "Complete"
			}
		}
		if err != nil {
			finished.State = StoreStateFailed
			if !errors.Is(err, context.Canceled) {
				slog.Warn("Codebase search background indexing failed", "project_root", options.ProjectRoot, "error", err)
			}
		}
		backgroundIndexes.Lock()
		current := backgroundIndexes.jobs[key]
		if current.id == jobID {
			backgroundIndexes.jobs[key] = backgroundIndexJob{status: finished, digest: digest, id: jobID}
		}
		backgroundIndexes.Unlock()
	}()
	return status
}

func ProjectIndexStatus(projectRoot, storeDirectory string) StoreStatus {
	return InspectProjectIndexStatus(ProjectIndexOptions{
		ProjectRoot:    projectRoot,
		StoreDirectory: storeDirectory,
		Enabled:        true,
	})
}

func ProjectIndexStatusWithFilters(projectRoot, storeDirectory string, filters ProjectFilters) StoreStatus {
	return InspectProjectIndexStatus(ProjectIndexOptions{
		ProjectRoot:    projectRoot,
		StoreDirectory: storeDirectory,
		Enabled:        true,
		Filters:        filters,
	})
}

func InspectProjectIndexStatus(options ProjectIndexOptions) StoreStatus {
	return inspectProjectIndexStatus(options, true)
}

func inspectProjectIndexStatus(options ProjectIndexOptions, checkNativeSource bool) StoreStatus {
	if !options.Enabled {
		return statusWithDetails(options, options.StoreDirectory, StoreStatus{State: StoreStateDisabled})
	}
	directory, err := resolveStoreDirectory(options.StoreDirectory)
	if err != nil {
		return statusWithDetails(options, options.StoreDirectory, StoreStatus{State: StoreStateFailed, Err: err})
	}
	options.Filters = NormalizeProjectFilters(options.Filters)
	filter := filterDigest(options.Filters)
	digest := projectIndexOptionsDigest(options)
	key := projectIndexKey(options.ProjectRoot, directory)
	backgroundIndexes.RLock()
	job, exists := backgroundIndexes.jobs[key]
	backgroundIndexes.RUnlock()
	jobMatches := exists && (job.digest == digest || options.ConfiguredDatabasePath == "" && strings.HasPrefix(job.digest, filter+"\x00"))

	catalog, catalogErr := loadProjectCatalog(directory, options.ProjectRoot)
	if catalogErr == nil {
		status := statusWithCatalog(options, directory, catalog, StoreStatus{State: StoreStateReady})
		if catalog.Source.FilterDigest != filter {
			status.State = StoreStateMissing
			status.Serving = false
			if jobMatches && (job.status.State == StoreStateIndexing || job.status.State == StoreStateFailed) {
				job.status.Serving = false
				return job.status
			}
			return status
		}
		if jobMatches && (job.status.State == StoreStateIndexing || job.status.State == StoreStateFailed) {
			job.status.Serving = true
			return job.status
		}
		if (catalog.Source.Mode != "native" || checkNativeSource) && !sourceFilesCurrent(catalog.Source) {
			status.State = StoreStateStale
			return status
		}
		if jobMatches && job.status.State == StoreStateReady {
			job.status = statusWithCatalog(options, directory, catalog, job.status)
			return job.status
		}
		return status
	}
	if jobMatches && (job.status.State == StoreStateIndexing || job.status.State == StoreStateFailed) {
		job.status.Serving = false
		return job.status
	}
	if errors.Is(catalogErr, os.ErrNotExist) {
		checkpoint, checkpointErr := loadLatestProjectCheckpoint(directory, options.ProjectRoot, options.ConfiguredDatabasePath, filter)
		if checkpointErr == nil {
			status := statusWithCatalog(options, directory, checkpoint, StoreStatus{State: StoreStateMissing})
			if status.Stage == "" {
				status.Stage = "Resume available"
			}
			return status
		}
		if !errors.Is(checkpointErr, os.ErrNotExist) {
			return statusWithDetails(options, directory, StoreStatus{State: StoreStateFailed, Err: checkpointErr})
		}
		return statusWithDetails(options, directory, StoreStatus{State: StoreStateMissing})
	}
	return statusWithDetails(options, directory, StoreStatus{State: StoreStateFailed, Err: catalogErr})
}

func statusWithDetails(options ProjectIndexOptions, directory string, status StoreStatus) StoreStatus {
	status.ProjectRoot = options.ProjectRoot
	status.DatabasePath = options.ConfiguredDatabasePath
	if status.DatabasePath == "" {
		status.DatabasePath, _ = DefaultDatabasePath(options.ProjectRoot)
	}
	status.StoreDirectory = directory
	if status.StoreDirectory == "" {
		status.StoreDirectory, _ = DefaultStoreDirectory()
	}
	token, err := CodebaseIndexToken(context.Background())
	switch {
	case err != nil:
		status.CredentialStatus = "invalid"
	case token == "":
		status.CredentialStatus = "missing"
	default:
		status.CredentialStatus = "signed-in"
	}
	return status
}

func statusWithCatalog(options ProjectIndexOptions, directory string, catalog storeCatalog, status StoreStatus) StoreStatus {
	status = statusWithDetails(options, directory, status)
	status.Serving = catalog.Complete
	status.SourceMode = catalog.Source.Mode
	status.Model = catalog.Model
	status.ChunksCreated = catalog.Chunks
	status.FilesTotal = catalog.FilesTotal
	status.FilesProcessed = catalog.FilesProcessed
	status.FilesSkipped = catalog.FilesSkipped
	status.CurrentPath = catalog.CurrentPath
	status.Stage = catalog.Stage
	status.StartedAt = catalog.StartedAt
	status.FinishedAt = catalog.IndexedAt
	if catalog.Source.Mode == "native" {
		if status.FilesTotal == 0 {
			status.FilesTotal = int(catalog.Source.ChunkCount)
		}
		if catalog.Complete {
			status.FilesProcessed = status.FilesTotal
		}
	} else if catalog.Source.DatabasePath != "" {
		status.DatabasePath = catalog.Source.DatabasePath
	}
	return status
}

func projectIndexKey(projectRoot, storeDirectory string) string {
	return projectRoot + "\x00" + storeDirectory
}

func projectIndexOptionsDigest(options ProjectIndexOptions) string {
	return filterDigest(options.Filters) + "\x00" + options.ConfiguredDatabasePath
}

func indexProjectWithFilters(ctx context.Context, projectRoot, configuredDatabasePath, storeDirectory string, filters ProjectFilters, report func(IndexProgress)) error {
	if report != nil {
		report(IndexProgress{Stage: "Checking index source"})
	}
	databasePath, found, err := findImportDatabase(ctx, projectRoot, configuredDatabasePath)
	if err != nil {
		return err
	}
	if !found {
		return runNativeProjectIndexing(ctx, projectRoot, storeDirectory, filters, report)
	}
	if report != nil {
		report(IndexProgress{Stage: "Importing database"})
	}

	reader, err := OpenWithFilters(ctx, databasePath, storeDirectory, filters)
	if err != nil {
		return err
	}
	defer reader.Close()

	model, err := reader.Model(ctx, projectRoot)
	if err != nil {
		return err
	}
	_, _, err = reader.prepareStore(ctx, projectRoot, model)
	return err
}

func IsStoreUnavailable(err error) bool {
	var unavailable *StoreUnavailableError
	return errors.As(err, &unavailable)
}
