package tools

import (
	"context"
	"fmt"

	"github.com/example-git/crux/internal/filepathext"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/permission"
)

func canonicalToolPath(workingDir, path string) (string, error) {
	resolved, err := fsext.CanonicalPath(filepathext.SmartJoin(workingDir, path))
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	return resolved, nil
}

func authorizeExternalPath(
	ctx context.Context,
	permissions permission.Service,
	workingDir string,
	path string,
	toolCallID string,
	toolName string,
	action string,
	description string,
	params any,
) (bool, error) {
	resolvedWorkingDir, err := canonicalToolPath(workingDir, ".")
	if err != nil {
		return false, err
	}
	if fsext.HasPrefix(path, resolvedWorkingDir) {
		return true, nil
	}
	if permissions == nil {
		return false, fmt.Errorf("permission service is required for access outside the working directory")
	}
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return false, fmt.Errorf("session ID is required for access outside the working directory")
	}
	return permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID:   sessionID,
		Path:        path,
		ToolCallID:  toolCallID,
		ToolName:    toolName,
		Action:      action,
		Description: description,
		Params:      params,
	})
}
