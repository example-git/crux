package backend

import "github.com/example-git/crux/internal/proto"

func (b *Backend) ListProjects(workspaceID string) ([]proto.ProjectInfo, error) {
	workspace, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	documents, err := b.projectService.List()
	if err != nil {
		return nil, err
	}
	active, hasActive, err := b.projectService.Active(workspace.Path)
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
	if slug == "" {
		return b.projectService.Disable(workspace.Path)
	}
	_, err = b.projectService.Activate(slug, workspace.Path)
	return err
}
