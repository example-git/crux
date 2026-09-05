package codebaseindex

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/lock"
)

const storeMaintenanceInterval = 6 * time.Hour

type storePruneResult struct {
	RemovedGenerations  int
	RetainedGenerations int
	SkippedProjects     int
}

type storeGenerationCheckpoint struct {
	directory string
	catalog   storeCatalog
	updatedAt time.Time
}

var storeMaintenance = struct {
	sync.Mutex
	lastRun map[string]time.Time
}{lastRun: make(map[string]time.Time)}

func runStoreMaintenance(directory string) {
	directory, err := resolveStoreDirectory(directory)
	if err != nil {
		slog.Warn("Could not resolve codebase index store for maintenance", "error", err)
		return
	}
	now := time.Now()
	storeMaintenance.Lock()
	if now.Sub(storeMaintenance.lastRun[directory]) < storeMaintenanceInterval {
		storeMaintenance.Unlock()
		return
	}
	storeMaintenance.lastRun[directory] = now
	storeMaintenance.Unlock()

	result, err := pruneStoreGenerations(directory)
	if err != nil {
		slog.Warn("Could not prune codebase index store", "store_directory", directory, "error", err)
		return
	}
	if result.RemovedGenerations > 0 {
		slog.Info("Pruned superseded codebase index generations",
			"store_directory", directory,
			"removed", result.RemovedGenerations,
			"retained", result.RetainedGenerations,
			"skipped_projects", result.SkippedProjects,
		)
	}
}

func pruneStoreGenerations(directory string) (storePruneResult, error) {
	directory, err := resolveStoreDirectory(directory)
	if err != nil {
		return storePruneResult{}, err
	}
	projects, err := storeGenerationProjects(directory)
	if err != nil {
		return storePruneResult{}, err
	}
	sort.Strings(projects)

	var result storePruneResult
	var firstErr error
	for _, projectRoot := range projects {
		projectResult, pruneErr := pruneProjectGenerations(directory, projectRoot)
		result.RemovedGenerations += projectResult.RemovedGenerations
		result.RetainedGenerations += projectResult.RetainedGenerations
		result.SkippedProjects += projectResult.SkippedProjects
		if pruneErr != nil && firstErr == nil {
			firstErr = pruneErr
		}
	}
	return result, firstErr
}

func storeGenerationProjects(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	projects := make(map[string]struct{})
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		switch {
		case !entry.IsDir() && strings.HasPrefix(entry.Name(), "project-") && strings.HasSuffix(entry.Name(), ".catalog.json"):
			catalog, loadErr := loadStoreCatalog(path)
			if loadErr == nil {
				projects[catalog.ProjectRoot] = struct{}{}
			}
		case entry.IsDir() && strings.HasPrefix(entry.Name(), "generation-"):
			catalog, loadErr := loadStoreCatalog(filepath.Join(path, "migration.json"))
			if loadErr == nil && directStoreGeneration(directory, catalog.Directory) && filepath.Clean(catalog.Directory) == filepath.Clean(path) {
				projects[catalog.ProjectRoot] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(projects))
	for projectRoot := range projects {
		result = append(result, projectRoot)
	}
	return result, nil
}

func pruneProjectGenerations(directory, projectRoot string) (storePruneResult, error) {
	catalogPath := projectCatalogPath(directory, projectRoot)
	buildLock := storeBuildLock(catalogPath)
	buildLock.Lock()
	defer buildLock.Unlock()

	release, err := lock.TryFile(catalogPath + ".lock")
	if errors.Is(err, lock.ErrContended) {
		return storePruneResult{SkippedProjects: 1}, nil
	}
	if err != nil {
		return storePruneResult{}, err
	}
	defer release()

	checkpoints, err := projectGenerationCheckpoints(directory, projectRoot)
	if err != nil {
		return storePruneResult{}, err
	}
	activeDirectory := ""
	if catalog, loadErr := loadProjectCatalog(directory, projectRoot); loadErr == nil && directStoreGeneration(directory, catalog.Directory) {
		activeDirectory = filepath.Clean(catalog.Directory)
	}

	retainedCheckpoint := ""
	var retainedAt time.Time
	for _, checkpoint := range checkpoints {
		if filepath.Clean(checkpoint.directory) == activeDirectory {
			continue
		}
		if activeDirectory != "" && checkpoint.catalog.Complete {
			continue
		}
		if retainedCheckpoint == "" || checkpoint.updatedAt.After(retainedAt) {
			retainedCheckpoint = filepath.Clean(checkpoint.directory)
			retainedAt = checkpoint.updatedAt
		}
	}

	result := storePruneResult{}
	var firstErr error
	for _, checkpoint := range checkpoints {
		clean := filepath.Clean(checkpoint.directory)
		if clean == activeDirectory || clean == retainedCheckpoint {
			result.RetainedGenerations++
			continue
		}
		releaseLease, err := lock.TryFile(generationLeasePath(checkpoint.directory))
		if errors.Is(err, lock.ErrContended) {
			result.RetainedGenerations++
			continue
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			result.RetainedGenerations++
			continue
		}
		if err := os.RemoveAll(checkpoint.directory); err != nil {
			releaseLease()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		releaseLease()
		result.RemovedGenerations++
	}
	return result, firstErr
}

func projectGenerationCheckpoints(directory, projectRoot string) ([]storeGenerationCheckpoint, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	result := make([]storeGenerationCheckpoint, 0)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), "generation-") {
			continue
		}
		generationDirectory := filepath.Join(directory, entry.Name())
		checkpointPath := filepath.Join(generationDirectory, "migration.json")
		catalog, loadErr := loadStoreCatalog(checkpointPath)
		if loadErr != nil || catalog.ProjectRoot != projectRoot || !directStoreGeneration(directory, catalog.Directory) || filepath.Clean(catalog.Directory) != filepath.Clean(generationDirectory) {
			continue
		}
		updatedAt := catalog.ProgressUpdatedAt
		if updatedAt.IsZero() {
			updatedAt = catalog.IndexedAt
		}
		if updatedAt.IsZero() {
			if info, statErr := os.Stat(checkpointPath); statErr == nil {
				updatedAt = info.ModTime()
			}
		}
		result = append(result, storeGenerationCheckpoint{
			directory: generationDirectory,
			catalog:   catalog,
			updatedAt: updatedAt,
		})
	}
	return result, nil
}

func generationLeasePath(generationDirectory string) string {
	return generationDirectory + ".read.lock"
}

func directStoreGeneration(storeDirectory, generationDirectory string) bool {
	storeDirectory, err := filepath.Abs(storeDirectory)
	if err != nil {
		return false
	}
	generationDirectory, err = filepath.Abs(generationDirectory)
	if err != nil {
		return false
	}
	return filepath.Dir(generationDirectory) == storeDirectory && strings.HasPrefix(filepath.Base(generationDirectory), "generation-")
}
