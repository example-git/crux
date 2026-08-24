package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/example-git/crux/internal/proto"
)

func (c *Client) CodebaseIndexStatus(ctx context.Context, workspaceID string) (proto.CodebaseIndexStatus, error) {
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/codebase-index", workspaceID), nil, nil)
	if err != nil {
		return proto.CodebaseIndexStatus{}, fmt.Errorf("failed to get codebase index status: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return proto.CodebaseIndexStatus{}, fmt.Errorf("failed to get codebase index status: %w", err)
	}
	var status proto.CodebaseIndexStatus
	if err := json.NewDecoder(rsp.Body).Decode(&status); err != nil {
		return proto.CodebaseIndexStatus{}, fmt.Errorf("failed to decode codebase index status: %w", err)
	}
	return status, nil
}

func (c *Client) UpdateCodebaseIndex(ctx context.Context, workspaceID string, update proto.CodebaseIndexUpdate) (proto.CodebaseIndexStatus, error) {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/codebase-index", workspaceID), nil, jsonBody(update), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return proto.CodebaseIndexStatus{}, fmt.Errorf("failed to update codebase index: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return proto.CodebaseIndexStatus{}, fmt.Errorf("failed to update codebase index: %w", err)
	}
	var status proto.CodebaseIndexStatus
	if err := json.NewDecoder(rsp.Body).Decode(&status); err != nil {
		return proto.CodebaseIndexStatus{}, fmt.Errorf("failed to decode codebase index status: %w", err)
	}
	return status, nil
}
