package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/skills"
)

const (
	SkillListToolName = "skill_list"
	SkillLoadToolName = "skill_load"
	maxSkillBytes     = 100_000
)

//go:embed skill_list.md
var skillListDescription string

//go:embed skill_load.md
var skillLoadDescription string

type SkillLoadParams struct {
	Name string `json:"name" description:"Name of the active skill to load"`
}

type skillListEntry struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Source        skills.SourceType `json:"source"`
	UserInvocable bool              `json:"user_invocable"`
	Loaded        bool              `json:"loaded"`
}

func NewSkillListTool(active []*skills.Skill, skillPaths []string, workingDir string, tracker *skills.Tracker) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		SkillListToolName,
		skillListDescription,
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			catalog := skills.Catalog(active, skillPaths, workingDir)
			entries := make([]skillListEntry, 0, len(catalog))
			for _, entry := range catalog {
				entries = append(entries, skillListEntry{
					Name:          entry.Name,
					Description:   entry.Description,
					Source:        entry.Source,
					UserInvocable: entry.UserInvocable,
					Loaded:        tracker.IsLoaded(entry.Name),
				})
			}
			slices.SortFunc(entries, func(left, right skillListEntry) int {
				return strings.Compare(left.Name, right.Name)
			})
			content, err := json.MarshalIndent(entries, "", "  ")
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("encoding skill catalog: %w", err)
			}
			return fantasy.NewTextResponse(string(content)), nil
		},
	)
}

func NewSkillLoadTool(active []*skills.Skill, skillPaths []string, workingDir string, tracker *skills.Tracker) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		SkillLoadToolName,
		skillLoadDescription,
		func(_ context.Context, params SkillLoadParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			name := strings.TrimSpace(params.Name)
			if name == "" {
				return fantasy.NewTextErrorResponse("name is required"), nil
			}
			var selected *skills.Skill
			for _, skill := range active {
				if skill.Name == name {
					selected = skill
					break
				}
			}
			if selected == nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("active skill not found: %s", name)), nil
			}
			content, metadata, err := skills.ReadContent(active, skillPaths, workingDir, selected.SkillFilePath)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(content) > maxSkillBytes {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("skill %q exceeds the %d-byte tool limit", name, maxSkillBytes)), nil
			}
			tracker.MarkLoaded(selected.Name)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(string(content)), metadata), nil
		},
	)
}
