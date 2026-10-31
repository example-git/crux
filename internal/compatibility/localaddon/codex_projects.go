package localaddon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/google/uuid"
)

type codexProjectRoot struct {
	Path string `json:"path"`
}

type codexProject struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Roots     []codexProjectRoot `json:"roots"`
	Metadata  map[string]string  `json:"metadata"`
	Position  int64              `json:"position"`
	CreatedAt int64              `json:"createdAt"`
	UpdatedAt int64              `json:"updatedAt"`
	RecencyAt *int64             `json:"recencyAt"`
}

type codexProjectRecord struct {
	Project        codexProject `json:"project"`
	IdempotencyKey string       `json:"idempotencyKey"`
	Deleted        bool         `json:"deleted,omitempty"`
}

func (b *codexNativeBridge) projectRecords(ctx context.Context) (map[string]codexProjectRecord, config.ConfigReadView, error) {
	api, err := codexClientFactory(b.request.WorkingDir)
	if err != nil {
		return nil, config.ConfigReadView{}, err
	}
	view, err := api.ReadConfigView(ctx, "")
	if err != nil {
		return nil, view, err
	}
	records := map[string]codexProjectRecord{}
	if value := view.Desktop["crux-projects"]; value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, view, err
		}
		if err := json.Unmarshal(encoded, &records); err != nil {
			return nil, view, fmt.Errorf("invalid persisted Desktop projects: %w", err)
		}
	}
	return records, view, nil
}

func (b *codexNativeBridge) saveProjectRecord(ctx context.Context, record codexProjectRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	api, err := codexClientFactory(b.request.WorkingDir)
	if err != nil {
		return err
	}
	_, err = api.WriteClientSettings(ctx, config.ClientSettingsWriteRequest{Edits: []config.ClientSettingsEdit{{KeyPath: "desktop.crux-projects." + record.Project.ID, Value: encoded, MergeStrategy: "replace"}}})
	return err
}

func canonicalProjectDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("project directory must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("project directory %q is inaccessible: %w", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project directory %q is not a directory", path)
	}
	return resolved, nil
}

func validateCodexProjectRoots(roots []codexProjectRoot) error {
	seen := map[string]bool{}
	for _, root := range roots {
		path, err := canonicalProjectDirectory(root.Path)
		if err != nil {
			return err
		}
		if seen[path] {
			return fmt.Errorf("duplicate project root %q", root.Path)
		}
		seen[path] = true
	}
	return nil
}

func (b *codexNativeBridge) defaultProjectDirectory(ctx context.Context) (string, error) {
	if b.defaultCWD != "" {
		return b.defaultCWD, nil
	}
	_, view, err := b.projectRecords(ctx)
	if err != nil {
		return "", err
	}
	if value, exists := view.Desktop["crux-default-cwd"]; exists {
		path, ok := value.(string)
		if !ok || path == "" {
			return "", errors.New("desktop.crux-default-cwd must be an absolute directory")
		}
		b.defaultCWD, err = canonicalProjectDirectory(path)
		return b.defaultCWD, err
	}
	home, err := codexHome(b.invocation.Env, b.request.WorkingDir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, "workspaces", "default")
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	b.defaultCWD, err = canonicalProjectDirectory(path)
	return b.defaultCWD, err
}

func (b *codexNativeBridge) projectForDirectory(ctx context.Context, cwd string) (string, error) {
	records, _, err := b.projectRecords(ctx)
	if err != nil {
		return "", err
	}
	path, err := canonicalProjectDirectory(cwd)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		record := records[id]
		if record.Deleted {
			continue
		}
		for _, root := range record.Project.Roots {
			resolved, resolveErr := filepath.EvalSymlinks(root.Path)
			if resolveErr == nil && resolved == path {
				return id, nil
			}
		}
	}
	return "", nil
}

func (b *codexNativeBridge) projectDirectories(ctx context.Context, projectID string) ([]string, error) {
	records, _, err := b.projectRecords(ctx)
	if err != nil {
		return nil, err
	}
	record, exists := records[projectID]
	if !exists || record.Deleted {
		return nil, fmt.Errorf("project not found: %s", projectID)
	}
	if len(record.Project.Roots) == 0 {
		return nil, fmt.Errorf("project %q has no configured directory", projectID)
	}
	paths := make([]string, 0, len(record.Project.Roots))
	for _, root := range record.Project.Roots {
		path, err := canonicalProjectDirectory(root.Path)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func (b *codexNativeBridge) resolveProjectCWD(ctx context.Context, cwd, projectID string) (string, error) {
	if projectID != "" {
		paths, err := b.projectDirectories(ctx, projectID)
		if err != nil {
			return "", err
		}
		if cwd == "" {
			cwd = paths[0]
		}
		path, err := canonicalProjectDirectory(cwd)
		if err != nil {
			return "", err
		}
		if !slicesContain(paths, path) {
			return "", fmt.Errorf("cwd %q is not a root of project %q", cwd, projectID)
		}
		b.activeCWD = path
		return path, nil
	}
	if cwd != "" {
		path, err := canonicalProjectDirectory(cwd)
		if err == nil {
			b.activeCWD = path
		}
		return path, err
	}
	if b.activeCWD != "" {
		return b.activeCWD, nil
	}
	return b.defaultProjectDirectory(ctx)
}

func slicesContain(paths []string, path string) bool {
	for _, candidate := range paths {
		if candidate == path {
			return true
		}
	}
	return false
}

func (b *codexNativeBridge) lookupProjectWorkspaces(ctx context.Context, cwd string) ([]*codexNativeWorkspace, error) {
	if cwd != "" {
		path, err := canonicalProjectDirectory(cwd)
		if err != nil {
			return nil, err
		}
		workspace, err := b.workspace(path)
		if err != nil {
			return nil, err
		}
		return []*codexNativeWorkspace{workspace}, nil
	}
	paths := map[string]bool{}
	for path := range b.workspaces {
		paths[path] = true
	}
	records, _, err := b.projectRecords(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Deleted {
			continue
		}
		for _, root := range record.Project.Roots {
			paths[root.Path] = true
		}
	}
	if b.activeCWD != "" {
		paths[b.activeCWD] = true
	}
	fallback, err := b.defaultProjectDirectory(ctx)
	if err != nil {
		return nil, err
	}
	paths[fallback] = true
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	workspaces := make([]*codexNativeWorkspace, 0, len(paths))
	for _, path := range ordered {
		workspace, err := b.workspace(path)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces, nil
}

func (b *codexNativeBridge) handleProject(ctx context.Context, method string, raw json.RawMessage) (map[string]any, *codexProject, string, error) {
	var params struct {
		ProjectID      string              `json:"projectId"`
		Threads        *[]string           `json:"threads"`
		Name           *string             `json:"name"`
		Roots          *[]codexProjectRoot `json:"roots"`
		Metadata       *map[string]string  `json:"metadata"`
		IdempotencyKey string              `json:"idempotencyKey"`
		Cursor         *string             `json:"cursor"`
		Limit          *uint32             `json:"limit"`
		SortKey        string              `json:"sortKey"`
		SortDirection  string              `json:"sortDirection"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, nil, "", err
		}
	}
	records, _, err := b.projectRecords(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	if method == "project/list" {
		if params.SortDirection != "" && params.SortKey == "" {
			return nil, nil, "", errors.New("sortDirection requires sortKey")
		}
		if params.SortKey != "" && params.SortKey != "position" && params.SortKey != "recencyAt" {
			return nil, nil, "", errors.New("invalid project sortKey")
		}
		if params.SortDirection != "" && params.SortDirection != "asc" && params.SortDirection != "desc" {
			return nil, nil, "", errors.New("invalid project sortDirection")
		}
		projects := make([]codexProject, 0)
		for _, record := range records {
			if !record.Deleted {
				projects = append(projects, record.Project)
			}
		}
		sort.Slice(projects, func(i, j int) bool {
			less := projects[i].Position < projects[j].Position
			if projects[i].Position == projects[j].Position {
				less = projects[i].ID < projects[j].ID
			}
			if params.SortDirection == "desc" {
				return !less && projects[i].ID != projects[j].ID
			}
			return less
		})
		start := 0
		if params.Cursor != nil {
			start, err = strconv.Atoi(*params.Cursor)
			if err != nil || start < 0 || start > len(projects) {
				return nil, nil, "", errors.New("invalid project cursor")
			}
		}
		limit := 25
		if params.Limit != nil {
			limit = int(min(max(*params.Limit, 1), 100))
		}
		end := min(start+limit, len(projects))
		var next any
		if end < len(projects) {
			next = strconv.Itoa(end)
		}
		return map[string]any{"data": projects[start:end], "nextCursor": next}, nil, "", nil
	}
	if method == "project/create" || method == "project/import" {
		if params.Name == nil || strings.TrimSpace(*params.Name) == "" || params.Roots == nil || strings.TrimSpace(params.IdempotencyKey) == "" {
			return nil, nil, "", errors.New("project creation requires name, roots and idempotencyKey")
		}
		for _, record := range records {
			if record.IdempotencyKey == params.IdempotencyKey {
				if record.Deleted {
					return nil, nil, "", errors.New("idempotency key refers to deleted project")
				}
				return map[string]any{"project": record.Project}, nil, "", nil
			}
		}
		if err := validateCodexProjectRoots(*params.Roots); err != nil {
			return nil, nil, "", err
		}
		if method == "project/import" && params.Threads != nil {
			for _, threadID := range *params.Threads {
				found := false
				for _, root := range *params.Roots {
					if _, _, err := b.findThread(ctx, threadID, root.Path); err == nil {
						found = true
						break
					}
				}
				if !found {
					return nil, nil, "", fmt.Errorf("imported thread %q does not belong to a project directory", threadID)
				}
			}
		}
		digest := sha256.Sum256([]byte(params.IdempotencyKey))
		var id uuid.UUID
		copy(id[:], digest[:16])
		id[6] = id[6]&0x0f | 0x70
		id[8] = id[8]&0x3f | 0x80
		now := time.Now().Unix()
		project := codexProject{ID: id.String(), Name: strings.TrimSpace(*params.Name), Roots: *params.Roots, Metadata: map[string]string{}, Position: now, CreatedAt: now, UpdatedAt: now}
		if params.Metadata != nil {
			project.Metadata = *params.Metadata
		}
		if err := b.saveProjectRecord(ctx, codexProjectRecord{Project: project, IdempotencyKey: params.IdempotencyKey}); err != nil {
			return nil, nil, "", err
		}
		return map[string]any{"project": project}, &project, "created", nil
	}
	record, exists := records[params.ProjectID]
	if !exists || record.Deleted {
		return nil, nil, "", fmt.Errorf("project not found: %s", params.ProjectID)
	}
	if method == "project/read" {
		return map[string]any{"project": record.Project}, nil, "", nil
	}
	if method == "project/delete" {
		record.Deleted = true
		if err := b.saveProjectRecord(ctx, record); err != nil {
			return nil, nil, "", err
		}
		return map[string]any{}, &record.Project, "deleted", nil
	}
	if method != "project/update" {
		return nil, nil, "", errors.New("project operation is unavailable")
	}
	before, _ := json.Marshal(record.Project)
	if params.Name != nil {
		if strings.TrimSpace(*params.Name) == "" {
			return nil, nil, "", errors.New("project name must not be empty")
		}
		record.Project.Name = strings.TrimSpace(*params.Name)
	}
	if params.Roots != nil {
		if err := validateCodexProjectRoots(*params.Roots); err != nil {
			return nil, nil, "", err
		}
		record.Project.Roots = *params.Roots
	}
	if params.Metadata != nil {
		record.Project.Metadata = *params.Metadata
	}
	after, _ := json.Marshal(record.Project)
	if string(before) == string(after) {
		return map[string]any{"project": record.Project}, nil, "", nil
	}
	record.Project.UpdatedAt = time.Now().Unix()
	if err := b.saveProjectRecord(ctx, record); err != nil {
		return nil, nil, "", err
	}
	return map[string]any{"project": record.Project}, &record.Project, "updated", nil
}

func (b *codexNativeBridge) listProjectThreads(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var params struct {
		CWD            json.RawMessage `json:"cwd"`
		ProjectID      json.RawMessage `json:"projectId"`
		ModelProviders []string        `json:"modelProviders"`
		Cursor         *string         `json:"cursor"`
		Limit          *uint32         `json:"limit"`
		SortKey        string          `json:"sortKey"`
		SortDirection  string          `json:"sortDirection"`
		SearchTerm     *string         `json:"searchTerm"`
		Archived       *bool           `json:"archived"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
	}
	var projectID string
	if len(params.ProjectID) > 0 && string(params.ProjectID) != "null" {
		if err := json.Unmarshal(params.ProjectID, &projectID); err != nil || projectID == "" {
			return nil, errors.New("projectId must be a nonempty string")
		}
	}
	var paths []string
	if len(params.CWD) > 0 && string(params.CWD) != "null" {
		var path string
		if json.Unmarshal(params.CWD, &path) == nil {
			paths = []string{path}
		} else if err := json.Unmarshal(params.CWD, &paths); err != nil {
			return nil, errors.New("cwd must be a path or array of paths")
		}
	}
	if projectID != "" {
		roots, err := b.projectDirectories(ctx, projectID)
		if err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			paths = roots
		} else {
			for _, path := range paths {
				canonical, err := canonicalProjectDirectory(path)
				if err != nil {
					return nil, err
				}
				if !slicesContain(roots, canonical) {
					return nil, fmt.Errorf("cwd %q is not a root of project %q", path, projectID)
				}
			}
		}
	}
	if len(paths) == 0 {
		path, err := b.resolveProjectCWD(ctx, "", "")
		if err != nil {
			return nil, err
		}
		paths = []string{path}
	}
	if len(params.ModelProviders) > 0 && !slicesContain(params.ModelProviders, "crux") {
		return map[string]any{"data": []any{}, "nextCursor": nil, "backwardsCursor": nil}, nil
	}
	entries := make([]codexThreadListEntry, 0)
	seen := map[string]bool{}
	for _, path := range paths {
		cwd, err := b.resolveProjectCWD(ctx, path, projectID)
		if err != nil {
			return nil, err
		}
		if seen[cwd] {
			continue
		}
		seen[cwd] = true
		workspace, err := b.workspace(cwd)
		if err != nil {
			return nil, err
		}
		if _, err := b.listModels(ctx, workspace); err != nil {
			return nil, err
		}
		var sessions []proto.Session
		if params.Archived != nil && *params.Archived {
			sessions, err = workspace.client.ListArchivedSessions(ctx, workspace.value.ID)
		} else {
			sessions, err = workspace.client.ListSessions(ctx, workspace.value.ID)
		}
		if err != nil {
			return nil, err
		}
		for index := range sessions {
			sess := &sessions[index]
			if params.SearchTerm != nil && !strings.Contains(sess.Title, *params.SearchTerm) {
				continue
			}
			binding := b.bindThread(workspace, sess)
			binding.projectID, err = b.projectForDirectory(ctx, workspace.value.Path)
			if err != nil {
				return nil, err
			}
			if len(params.ProjectID) > 0 && string(params.ProjectID) == "null" && binding.projectID != "" {
				continue
			}
			if err := b.hydrateThreadLineage(ctx, binding, sess); err != nil {
				return nil, err
			}
			entries = append(entries, codexThreadListEntry{thread: codexThread(binding, sess, binding.model), id: binding.codexID, createdAt: sess.CreatedAt, updatedAt: sess.UpdatedAt})
		}
	}
	page, next, backwards, err := paginateCodexThreads(entries, params.Cursor, params.Limit, params.SortKey, params.SortDirection)
	if err != nil {
		return nil, err
	}
	return map[string]any{"data": page, "nextCursor": next, "backwardsCursor": backwards}, nil
}

func codexNullableProjectID(id string) any {
	if id == "" {
		return nil
	}
	return id
}
