package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
)

const (
	JobOutputToolName = "job_output"
)

//go:embed job_output.md
var jobOutputDescription string

type JobOutputParams struct {
	ShellID string `json:"shell_id" description:"The ID of the background shell to retrieve output from"`
	Wait    bool   `json:"wait" description:"If true, block until the background shell completes before returning output"`
}

type JobOutputResponseMetadata struct {
	ShellID          string `json:"shell_id"`
	Command          string `json:"command"`
	Description      string `json:"description"`
	Done             bool   `json:"done"`
	WorkingDirectory string `json:"working_directory"`
}

func NewJobOutputTool(service TaskService, backgroundShells *shell.BackgroundShellManager) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		JobOutputToolName,
		jobOutputDescription,
		func(ctx context.Context, params JobOutputParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.ShellID == "" {
				return fantasy.NewTextErrorResponse("missing shell_id"), nil
			}

			result, err := service.TaskOutput(ctx, params.ShellID, params.Wait, managedtask.DefaultOutputWait)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			outputParts := make([]string, 0, 2)
			if result.Output != "" {
				outputParts = append(outputParts, result.Output)
			}
			if result.Task.State.ExitCode != nil && *result.Task.State.ExitCode != 0 {
				outputParts = append(outputParts, fmt.Sprintf("Exit code %d", *result.Task.State.ExitCode))
			}
			output := TruncateOutput(strings.Join(outputParts, "\n"))
			if output == "" {
				output = BashNoOutput
			}

			metadata := JobOutputResponseMetadata{
				ShellID:     result.Task.ID,
				Description: result.Task.Description,
				Done:        result.Task.State.Status.Terminal(),
			}
			if backgroundShell, ok := backgroundShells.Get(params.ShellID); ok {
				metadata.Command = backgroundShell.Command
				metadata.WorkingDirectory = backgroundShell.WorkingDir
			}

			response := fmt.Sprintf("Status: %s\n\n%s", result.Task.State.Status, output)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
		},
	)
}
