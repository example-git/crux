package task

import (
	"context"
	"crypto/rand"
	"fmt"
)

type Type string

const (
	TypeShell Type = "shell"
	TypeAgent Type = "agent"
	TypeImage Type = "image"
)

const (
	idRandomLength = 8
	idAlphabet     = "0123456789abcdefghijklmnopqrstuvwxyz"
)

type Ownership struct {
	WorkspaceID      string `json:"workspace_id"`
	ParentSessionID  string `json:"parent_session_id"`
	OwnerAgentTaskID string `json:"owner_agent_task_id,omitempty"`
	OriginToolCallID string `json:"origin_tool_call_id,omitempty"`
}

type ownershipContextKey struct{}

func WithOwnership(ctx context.Context, ownership Ownership) context.Context {
	return context.WithValue(ctx, ownershipContextKey{}, ownership)
}

func OwnershipFromContext(ctx context.Context) Ownership {
	ownership, _ := ctx.Value(ownershipContextKey{}).(Ownership)
	return ownership
}

func NewID(taskType Type) (string, error) {
	prefix, err := prefixForType(taskType)
	if err != nil {
		return "", err
	}
	bytes := make([]byte, idRandomLength)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generating task ID: %w", err)
	}
	id := make([]byte, idRandomLength+1)
	id[0] = prefix
	for i, value := range bytes {
		id[i+1] = idAlphabet[int(value)%len(idAlphabet)]
	}
	return string(id), nil
}

func ParseID(id string) (Type, error) {
	if len(id) != idRandomLength+1 {
		return "", fmt.Errorf("invalid task ID %q", id)
	}
	var taskType Type
	switch id[0] {
	case 'b':
		taskType = TypeShell
	case 'a':
		taskType = TypeAgent
	case 'i':
		taskType = TypeImage
	default:
		return "", fmt.Errorf("invalid task ID %q", id)
	}
	for _, char := range id[1:] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'z') {
			return "", fmt.Errorf("invalid task ID %q", id)
		}
	}
	return taskType, nil
}

func prefixForType(taskType Type) (byte, error) {
	switch taskType {
	case TypeShell:
		return 'b', nil
	case TypeAgent:
		return 'a', nil
	case TypeImage:
		return 'i', nil
	default:
		return 0, fmt.Errorf("invalid task type %q", taskType)
	}
}
