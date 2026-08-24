package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/projects"
)

const (
	ProjectCreateToolName   = "project_create"
	ProjectStatusToolName   = "project_status"
	ProjectUpdateToolName   = "project_update"
	ProjectNotesToolName    = "project_notes"
	ProjectCompleteToolName = "project_complete"
)

//go:embed project_create.md
var projectCreateDescription string

//go:embed project_status.md
var projectStatusDescription string

//go:embed project_update.md
var projectUpdateDescription string

//go:embed project_notes.md
var projectNotesDescription string

//go:embed project_complete.md
var projectCompleteDescription string

type ProjectCreateParams struct {
	Name            string                  `json:"name" description:"Human-readable project name"`
	Slug            string                  `json:"slug" description:"Stable lowercase project slug using letters, numbers, and hyphens"`
	Goal            string                  `json:"goal" description:"Durable project goal"`
	SuccessCriteria []string                `json:"success_criteria" description:"Measurable conditions required for project completion"`
	Tasks           []ProjectDefinitionTask `json:"tasks" description:"Ordered project tasks and subtasks"`
}

type ProjectDefinitionTask struct {
	ID       string `json:"id" description:"Stable task ID such as T1 or T1.1"`
	Content  string `json:"content" description:"Single-line task description"`
	ParentID string `json:"parent_id,omitempty" description:"Optional ID of an earlier parent task"`
}

type ProjectUpdateParams struct {
	ID        string `json:"id" description:"Task or success-criterion ID to update"`
	Completed bool   `json:"completed" description:"Whether the item is complete"`
	Note      string `json:"note,omitempty" description:"Optional durable evidence or progress note"`
}

type ProjectNotesParams struct {
	Content string `json:"content" description:"Markdown content to append to the active project's notes file"`
}

type projectStatusResponse struct {
	Name            string          `json:"name"`
	Slug            string          `json:"slug"`
	Status          projects.Status `json:"status"`
	Tasks           []projects.Task `json:"tasks"`
	CurrentGoal     *projects.Task  `json:"current_goal,omitempty"`
	CurrentSubtasks []projects.Task `json:"current_subtasks,omitempty"`
}

func NewProjectCreateTool(service *projects.Service, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ProjectCreateToolName,
		projectCreateDescription,
		func(_ context.Context, params ProjectCreateParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			tasks := make([]projects.DefinitionTask, len(params.Tasks))
			for index, task := range params.Tasks {
				tasks[index] = projects.DefinitionTask{ID: task.ID, Content: task.Content, ParentID: task.ParentID}
			}
			document, err := service.Create(projects.Definition{
				Name:            params.Name,
				Slug:            params.Slug,
				Goal:            params.Goal,
				SuccessCriteria: params.SuccessCriteria,
				Tasks:           tasks,
			}, workingDir)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Created and activated project %q at %s with notes at %s.", document.Metadata.Name, document.Path, document.NotesPath)), nil
		},
	)
}

func NewProjectStatusTool(service *projects.Service, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ProjectStatusToolName,
		projectStatusDescription,
		func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			document, ok, err := service.Active(workingDir)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if !ok {
				return fantasy.NewTextResponse("No project is active for this workspace."), nil
			}
			response := projectStatusResponse{
				Name:   document.Metadata.Name,
				Slug:   document.Metadata.Slug,
				Status: document.Metadata.Status,
				Tasks:  document.Tasks,
			}
			if goal, subtasks, found := document.CurrentGoal(); found {
				response.CurrentGoal = &goal
				response.CurrentSubtasks = subtasks
			}
			content, err := json.MarshalIndent(response, "", "  ")
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			return fantasy.NewTextResponse(string(content)), nil
		},
	)
}

func NewProjectUpdateTool(service *projects.Service, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ProjectUpdateToolName,
		projectUpdateDescription,
		func(_ context.Context, params ProjectUpdateParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			document, err := service.UpdateTask(workingDir, params.ID, params.Completed, params.Note)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Updated %s in project %q.", params.ID, document.Metadata.Slug)), nil
		},
	)
}

func NewProjectNotesTool(service *projects.Service, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ProjectNotesToolName,
		projectNotesDescription,
		func(_ context.Context, params ProjectNotesParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			document, err := service.AppendNotes(workingDir, params.Content)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Appended notes to project %q.", document.Metadata.Slug)), nil
		},
	)
}

func NewProjectCompleteTool(service *projects.Service, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ProjectCompleteToolName,
		projectCompleteDescription,
		func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			document, err := service.Complete(workingDir)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Completed project %q and disabled it for this workspace.", document.Metadata.Slug)), nil
		},
	)
}
