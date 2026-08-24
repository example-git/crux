package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/example-git/crux/internal/proto"
)

func (c *Client) ListProjects(ctx context.Context, workspaceID string) ([]proto.ProjectInfo, error) {
	response, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/projects", workspaceID), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	defer response.Body.Close()
	if err := checkStatus(response); err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	var result []proto.ProjectInfo
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding projects: %w", err)
	}
	return result, nil
}

func (c *Client) SelectProject(ctx context.Context, workspaceID, slug string) error {
	response, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/projects/selection", workspaceID), nil, jsonBody(proto.ProjectSelectionRequest{Slug: slug}), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return fmt.Errorf("selecting project: %w", err)
	}
	defer response.Body.Close()
	if err := checkStatus(response); err != nil {
		return fmt.Errorf("selecting project: %w", err)
	}
	return nil
}
