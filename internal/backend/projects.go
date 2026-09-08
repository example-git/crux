package backend

import (
	"github.com/example-git/crux/internal/projects"
	"github.com/example-git/crux/internal/proto"
	"path/filepath"
)

func (b *Backend) ListProjects(workspaceID string) ([]proto.ProjectInfo, error) {
	workspace, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	service := b.workspaceProjectService(workspace)
	documents, err := service.List()
	if err != nil {
		return nil, err
	}
	active, hasActive, err := service.Active(workspace.Path)
	if err != nil {
		return nil, err
	}
	result := make([]proto.ProjectInfo, len(documents))
	for index, document := range documents {
		completed := 0
		for _, task := range document.Tasks {
			if task.Completed {
				completed++
			}
		}
		result[index] = proto.ProjectInfo{
			Slug:      document.Metadata.Slug,
			Name:      document.Metadata.Name,
			Status:    string(document.Metadata.Status),
			Selected:  hasActive && active.Metadata.Slug == document.Metadata.Slug,
			Completed: completed,
			Total:     len(document.Tasks),
		}
	}
	return result, nil
}

func (b *Backend) SelectProject(workspaceID, slug string) error {
	workspace, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}
	service := b.workspaceProjectService(workspace)
	if slug == "" {
		return service.Disable(workspace.Path)
	}
	_, err = service.Activate(slug, workspace.Path)
	return err
}

func (b *Backend) workspaceProjectService(workspace *Workspace) *projects.Service {
	if workspace.Cfg != nil && workspace.Cfg.RemoteAuthority() != nil {
		return projects.NewServiceAt(filepath.Join(workspace.Cfg.Config().Options.DataDirectory, "projects"))
	}
	return b.projectService
}
