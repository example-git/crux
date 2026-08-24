package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/automemory"
)

const (
	MemoryListToolName   = "memory_list"
	MemoryUpsertToolName = "memory_upsert"
	MemoryRemoveToolName = "memory_remove"
)

//go:embed memory_list.md
var memoryListDescription string

//go:embed memory_upsert.md
var memoryUpsertDescription string

//go:embed memory_remove.md
var memoryRemoveDescription string

type MemoryListParams struct {
	Scope string `json:"scope" description:"Memory scope: project or user"`
	Topic string `json:"topic,omitempty" description:"Optional topic file or ID to read in full"`
}

type MemoryUpsertParams struct {
	Scope       string `json:"scope" description:"Memory scope: project or user"`
	Topic       string `json:"topic" description:"Stable semantic topic ID using letters, numbers, hyphens, or underscores"`
	Name        string `json:"name" description:"Human-readable memory title"`
	Description string `json:"description" description:"Specific one-line relevance hook"`
	Type        string `json:"type" description:"Memory type: user, feedback, project, or reference"`
	Content     string `json:"content" description:"Durable memory body without frontmatter"`
}

type MemoryRemoveParams struct {
	Scope string `json:"scope" description:"Memory scope: project or user"`
	Topic string `json:"topic" description:"Topic file or ID to remove"`
}

func NewMemoryListTool(memory *automemory.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		MemoryListToolName,
		memoryListDescription,
		func(ctx context.Context, params MemoryListParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			scope, err := memoryScope(params.Scope)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if strings.TrimSpace(params.Topic) != "" {
				entry, err := memory.Get(ctx, scope, params.Topic)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				return memoryJSONResponse(entry)
			}
			entries, err := memory.List(ctx, scope)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return memoryJSONResponse(entries)
		},
	)
}

func NewMemoryUpsertTool(memory *automemory.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		MemoryUpsertToolName,
		memoryUpsertDescription,
		func(ctx context.Context, params MemoryUpsertParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			scope, err := memoryScope(params.Scope)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			entry, err := memory.Upsert(ctx, scope, automemory.Entry{
				File:        params.Topic,
				Name:        params.Name,
				Description: params.Description,
				Type:        params.Type,
				Content:     params.Content,
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Saved %s memory %s.", scope, entry.File)), nil
		},
	)
}

func NewMemoryRemoveTool(memory *automemory.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		MemoryRemoveToolName,
		memoryRemoveDescription,
		func(ctx context.Context, params MemoryRemoveParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			scope, err := memoryScope(params.Scope)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if err := memory.Remove(ctx, scope, params.Topic); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Removed %s memory %s.", scope, strings.TrimSpace(params.Topic))), nil
		},
	)
}

func memoryScope(value string) (automemory.Scope, error) {
	scope := automemory.Scope(strings.TrimSpace(value))
	switch scope {
	case automemory.ScopeProject, automemory.ScopeUser:
		return scope, nil
	default:
		return "", fmt.Errorf("invalid memory scope %q: expected project or user", value)
	}
}

func memoryJSONResponse(value any) (fantasy.ToolResponse, error) {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("encoding memory response: %w", err)
	}
	return fantasy.NewTextResponse(string(content)), nil
}
