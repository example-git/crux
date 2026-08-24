package backend

import (
	"strings"

	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/codebaseindex"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
)

func (b *Backend) CodebaseIndexStatus(workspaceID string) (proto.CodebaseIndexStatus, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.CodebaseIndexStatus{}, err
	}
	if ws.AgentCoordinator == nil {
		return proto.CodebaseIndexStatus{}, ErrAgentNotInitialized
	}
	status, err := agent.CodebaseIndexStatus(ws.ctx, ws.AgentCoordinator)
	if err != nil {
		return proto.CodebaseIndexStatus{}, err
	}
	result := codebaseIndexStatusProto(ws.Cfg.Config().Tools.CodebaseSearch, status)
	result.MemoryActivity = agent.AutoMemoryActivity(ws.AgentCoordinator)
	return result, nil
}

func (b *Backend) UpdateCodebaseIndex(workspaceID string, update proto.CodebaseIndexUpdate) (proto.CodebaseIndexStatus, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.CodebaseIndexStatus{}, err
	}
	if ws.AgentCoordinator == nil {
		return proto.CodebaseIndexStatus{}, ErrAgentNotInitialized
	}
	filters := codebaseindex.NormalizeProjectFilters(codebaseindex.ProjectFilters{
		IncludePaths: update.IncludePaths,
		ExcludePaths: update.ExcludePaths,
	})
	if err := ws.Cfg.SetConfigFields(config.ScopeWorkspace, map[string]any{
		"tools.codebase_search.enabled":         update.Enabled,
		"tools.codebase_search.database_path":   strings.TrimSpace(update.DatabasePath),
		"tools.codebase_search.store_directory": strings.TrimSpace(update.StoreDirectory),
		"tools.codebase_search.include_paths":   filters.IncludePaths,
		"tools.codebase_search.exclude_paths":   filters.ExcludePaths,
	}); err != nil {
		return proto.CodebaseIndexStatus{}, err
	}
	publishConfigChanged(ws)
	if err := ws.AgentCoordinator.UpdateModels(ws.ctx); err != nil {
		return proto.CodebaseIndexStatus{}, err
	}
	if update.Reindex {
		status, err := agent.ReconcileCodebaseIndex(ws.ctx, ws.AgentCoordinator)
		if err != nil {
			return proto.CodebaseIndexStatus{}, err
		}
		result := codebaseIndexStatusProto(ws.Cfg.Config().Tools.CodebaseSearch, status)
		result.MemoryActivity = agent.AutoMemoryActivity(ws.AgentCoordinator)
		return result, nil
	}
	return b.CodebaseIndexStatus(workspaceID)
}

func codebaseIndexStatusProto(settings config.ToolCodebaseSearch, status codebaseindex.StoreStatus) proto.CodebaseIndexStatus {
	result := proto.CodebaseIndexStatus{
		Enabled:          settings.IsEnabled(),
		State:            string(status.State),
		ProjectRoot:      status.ProjectRoot,
		DatabasePath:     status.DatabasePath,
		StoreDirectory:   status.StoreDirectory,
		SourceMode:       status.SourceMode,
		CredentialStatus: status.CredentialStatus,
		Model:            status.Model,
		IncludePaths:     append([]string(nil), settings.IncludePaths...),
		ExcludePaths:     append([]string(nil), settings.ExcludePaths...),
		FilesTotal:       status.FilesTotal,
		FilesProcessed:   status.FilesProcessed,
		ChunksCreated:    status.ChunksCreated,
		FilesSkipped:     status.FilesSkipped,
		CurrentPath:      status.CurrentPath,
		Stage:            status.Stage,
		StartedAt:        status.StartedAt,
		FinishedAt:       status.FinishedAt,
	}
	if status.Err != nil {
		result.Error = status.Err.Error()
	}
	return result
}
