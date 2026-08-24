package tools

import (
	"context"
	_ "embed"
	"fmt"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/shell"
)

const (
	JobKillToolName = "job_kill"
)

//go:embed job_kill.md
var jobKillDescription string

type JobKillParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to terminate"`
}

type JobKillResponseMetadata struct {
	ShellID     string `json:"shell_id"`
	Command     string `json:"command"`
	Description string `json:"description"`
}

func NewJobKillTool(service TaskService, backgroundShells *shell.BackgroundShellManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		JobKillToolName,
		jobKillDescription,
		func(ctx context.Context, params JobKillParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ShellID == "" {
				return fantasy.NewTextErrorResponse("missing shell_id"), nil
			}

			metadata := JobKillResponseMetadata{ShellID: params.ShellID}
			if backgroundShell, ok := backgroundShells.Get(params.ShellID); ok {
				metadata.Command = backgroundShell.Command
				metadata.Description = backgroundShell.Description
			}

			task, err := service.StopTask(ctx, params.ShellID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if metadata.Description == "" {
				metadata.Description = task.Description
			}

			result := fmt.Sprintf("Background shell %s is %s", task.ID, task.State.Status)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}
