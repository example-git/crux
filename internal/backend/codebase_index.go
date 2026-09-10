package backend

import (
	"errors"
	"reflect"
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
	var result proto.CodebaseIndexStatus
	err = ws.Cfg.WithRuntimeSnapshot(func(snapshot config.RuntimeSnapshot) error {
		coordinator := ws.CurrentAgentCoordinator()
		if coordinator == nil {
			return ErrAgentNotInitialized
		}
		status, err := agent.CodebaseIndexStatus(ws.ctx, coordinator, snapshot)
		if err != nil {
			return err
		}
		result = codebaseIndexStatusProto(snapshot.Config().Tools.CodebaseSearch, status)
		result.MemoryActivity = agent.AutoMemoryActivity(coordinator)
		return nil
	})
	return result, err
}

func (b *Backend) UpdateCodebaseIndex(workspaceID string, update proto.CodebaseIndexUpdate, attachments ...*proto.WorkspaceAttachment) (proto.CodebaseIndexStatus, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.CodebaseIndexStatus{}, err
	}
	coordinator := ws.CurrentAgentCoordinator()
	if coordinator == nil {
		return proto.CodebaseIndexStatus{}, ErrAgentNotInitialized
	}
	filters := codebaseindex.NormalizeProjectFilters(codebaseindex.ProjectFilters{
		IncludePaths: update.IncludePaths,
		ExcludePaths: update.ExcludePaths,
	})
	if ws.Cfg.RemoteAuthority() != nil {
		var result proto.CodebaseIndexStatus
		err := ws.Cfg.WithRuntimeSnapshot(func(snapshot config.RuntimeSnapshot) error {
			authority := snapshot.RemoteAuthority()
			if err := snapshot.RuntimeRevocation(); err != nil {
				return err
			}
			if authority == nil || len(attachments) == 0 || attachments[0] == nil || attachments[0].Mode != "client" || attachments[0].Revision != authority.Revision || attachments[0].Digest != authority.Digest {
				return config.ErrRemoteRuntimeRevision
			}
			cfg := snapshot.Config()
			settings := cfg.Tools.CodebaseSearch
			requested := config.ResolveRemoteCodebaseIndexSettings(config.ToolCodebaseSearch{Enabled: &update.Enabled, DatabasePath: update.DatabasePath, StoreDirectory: update.StoreDirectory}, ws.Path, cfg.Options.DataDirectory)
			acceptedFilters := codebaseindex.NormalizeProjectFilters(codebaseindex.ProjectFilters{IncludePaths: settings.IncludePaths, ExcludePaths: settings.ExcludePaths})
			if update.Enabled != settings.IsEnabled() || requested.DatabasePath != settings.DatabasePath || requested.GetStoreDirectory() != settings.GetStoreDirectory() || !reflect.DeepEqual(filters, acceptedFilters) {
				return errors.New("codebase index settings must be accepted through the owning client runtime")
			}
			coordinator := ws.CurrentAgentCoordinator()
			if coordinator == nil {
				return ErrAgentNotInitialized
			}
			var status codebaseindex.StoreStatus
			var err error
			if update.Reindex {
				status, err = agent.ReconcileCodebaseIndex(ws.ctx, coordinator, snapshot)
			} else {
				status, err = agent.CodebaseIndexStatus(ws.ctx, coordinator, snapshot)
			}
			if err != nil {
				return err
			}
			result = codebaseIndexStatusProto(settings, status)
			result.MemoryActivity = agent.AutoMemoryActivity(coordinator)
			return nil
		})
		return result, err
	} else {
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
		if err := coordinator.UpdateModels(ws.ctx); err != nil {
			return proto.CodebaseIndexStatus{}, err
		}
	}
	if update.Reindex {
		status, err := agent.ReconcileCodebaseIndex(ws.ctx, coordinator)
		if err != nil {
			return proto.CodebaseIndexStatus{}, err
		}
		result := codebaseIndexStatusProto(ws.Cfg.Config().Tools.CodebaseSearch, status)
		result.MemoryActivity = agent.AutoMemoryActivity(coordinator)
		return result, nil
	}
	return b.CodebaseIndexStatus(workspaceID)
}

func codebaseIndexStatusProto(settings config.ToolCodebaseSearch, status codebaseindex.StoreStatus) proto.CodebaseIndexStatus {
	result := proto.CodebaseIndexStatus{
		ConfiguredDatabasePath:   settings.DatabasePath,
		ConfiguredStoreDirectory: settings.GetStoreDirectory(),
		Enabled:                  settings.IsEnabled(),
		State:                    string(status.State),
		Serving:                  status.Serving,
		ProjectRoot:              status.ProjectRoot,
		DatabasePath:             status.DatabasePath,
		StoreDirectory:           status.StoreDirectory,
		SourceMode:               status.SourceMode,
		CredentialStatus:         status.CredentialStatus,
		Model:                    status.Model,
		IncludePaths:             append([]string(nil), settings.IncludePaths...),
		ExcludePaths:             append([]string(nil), settings.ExcludePaths...),
		FilesTotal:               status.FilesTotal,
		FilesProcessed:           status.FilesProcessed,
		ChunksCreated:            status.ChunksCreated,
		FilesSkipped:             status.FilesSkipped,
		CurrentPath:              status.CurrentPath,
		Stage:                    status.Stage,
		StartedAt:                status.StartedAt,
		FinishedAt:               status.FinishedAt,
	}
	if status.Err != nil {
		result.Error = status.Err.Error()
	}
	return result
}
