package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
)

const JobListToolName = "job_list"

//go:embed job_list.md
var jobListDescription string

type jobListEntry struct {
	ShellID          string `json:"shell_id"`
	Command          string `json:"command"`
	Description      string `json:"description"`
	Status           string `json:"status"`
	WorkingDirectory string `json:"working_directory"`
}

func NewJobListTool(service TaskService, manager *shell.BackgroundShellManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		JobListToolName,
		jobListDescription,
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			tasks := service.ListTasks()
			entries := make([]jobListEntry, 0, len(tasks))
			for _, task := range tasks {
				if task.Type != managedtask.TypeShell {
					continue
				}
				entry := jobListEntry{
					ShellID:     task.ID,
					Description: task.Description,
					Status:      string(task.State.Status),
				}
				if job, ok := manager.Get(task.ID); ok {
					entry.Command = job.Command
					entry.WorkingDirectory = job.WorkingDir
				}
				entries = append(entries, entry)
			}
			slices.SortFunc(entries, func(a, b jobListEntry) int {
				return strings.Compare(a.ShellID, b.ShellID)
			})
			content, err := json.MarshalIndent(entries, "", "  ")
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("encoding background jobs: %w", err)
			}
			if strings.TrimSpace(string(content)) == "[]" {
				return fantasy.NewTextResponse("No background jobs are currently tracked."), nil
			}
			return fantasy.NewTextResponse(string(content)), nil
		},
	)
}
