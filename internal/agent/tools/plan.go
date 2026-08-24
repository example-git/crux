package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/session"
)

const (
	EnterPlanToolName    = "enter_plan"
	ExitPlanToolName     = "exit_plan"
	CompletePlanToolName = "complete_plan"
)

//go:embed enter_plan.md
var enterPlanDescription string

//go:embed exit_plan.md
var exitPlanDescription string

//go:embed complete_plan.md
var completePlanDescription string

type ExitPlanParams struct {
	Plan string `json:"plan" description:"The complete proposed implementation plan in Markdown"`
}

type CompletePlanParams struct {
	Summary string `json:"summary" description:"A concise summary of the completed implementation and validation"`
}

func NewEnterPlanTool(sessions session.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		EnterPlanToolName,
		enterPlanDescription,
		func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for entering plan mode")
			}
			current, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}
			if current.Mode.IsPlan() {
				return fantasy.NewTextResponse("A plan lifecycle is already active."), nil
			}
			if err := sessions.SetMode(ctx, sessionID, session.ModePlan); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to enter plan mode: %w", err)
			}
			return fantasy.NewTextResponse("Plan mode enabled. Continue with read-only investigation, then present the complete plan and call exit_plan for review."), nil
		},
	)
}

func NewCompletePlanTool(sessions session.Service, questions question.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		CompletePlanToolName,
		completePlanDescription,
		func(ctx context.Context, params CompletePlanParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for completing a plan")
			}
			summary := strings.TrimSpace(params.Summary)
			if summary == "" {
				return fantasy.NewTextErrorResponse("completion summary is required"), nil
			}
			current, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}
			if current.Mode != session.ModePlanExecution {
				return fantasy.NewTextErrorResponse("an approved plan is not being executed"), nil
			}

			answers, err := questions.Ask(ctx, question.Request{
				SessionID:  sessionID,
				ToolCallID: call.ID,
				Questions: []question.Question{{
					Type:        question.TypeSingleChoice,
					Label:       "Completion review",
					Text:        "Is the approved plan complete?",
					Description: "Review the implementation summary and validation above. Approve completion to restore normal mode, or request remaining work in the free-text option.",
					Choices: []question.Choice{
						{ID: "approve", Label: "Approve completed work"},
						{ID: "continue", Label: "Continue working"},
					},
				}},
			})
			if err != nil {
				if errors.Is(err, question.ErrCancelled) {
					response := fantasy.NewTextErrorResponse("Completion review cancelled. Continue executing the approved plan.")
					response.StopTurn = true
					return response, nil
				}
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(answers) != 1 {
				return fantasy.NewTextErrorResponse("completion review returned an invalid answer"), nil
			}
			answer := answers[0]
			if remaining := strings.TrimSpace(answer.FillInText); remaining != "" {
				return fantasy.NewTextResponse("Completion was not approved. Continue executing the persisted plan and address this feedback before calling complete_plan again.\n\n" + remaining), nil
			}
			if len(answer.SelectedIDs) != 1 {
				return fantasy.NewTextErrorResponse("select completion approval, continue working, or provide remaining-work instructions"), nil
			}
			switch answer.SelectedIDs[0] {
			case "approve":
				if err := sessions.SetPlanState(ctx, sessionID, session.ModeDefault, ""); err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("failed to complete plan: %w", err)
				}
				response := fantasy.NewTextResponse("Completed work approved. The plan lifecycle is finished and normal instructions are restored.")
				response.StopTurn = true
				return response, nil
			case "continue":
				return fantasy.NewTextResponse("Completion was not approved. Continue executing the persisted plan, then call complete_plan again."), nil
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid completion review selection %q", answer.SelectedIDs[0])), nil
			}
		},
	)
}

func NewExitPlanTool(sessions session.Service, questions question.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ExitPlanToolName,
		exitPlanDescription,
		func(ctx context.Context, params ExitPlanParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for exiting plan mode")
			}
			plan := strings.TrimSpace(params.Plan)
			if plan == "" {
				return fantasy.NewTextErrorResponse("plan is required"), nil
			}
			current, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}
			if current.Mode != session.ModePlan && current.Mode != session.ModePlanRevision {
				return fantasy.NewTextErrorResponse("plan drafting or revision mode is not active"), nil
			}
			if err := sessions.SetPlanState(ctx, sessionID, session.ModePlanRevision, plan); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to begin plan review: %w", err)
			}

			answers, err := questions.Ask(ctx, question.Request{
				SessionID:  sessionID,
				ToolCallID: call.ID,
				Questions: []question.Question{{
					Type:        question.TypeSingleChoice,
					Label:       "Plan review",
					Text:        "What should happen with this plan?",
					Description: "Review the proposed plan in the assistant message above. Approve it to begin execution, reject it to stop, or type revision instructions in the free-text option.",
					Choices: []question.Choice{
						{ID: "approve", Label: "Approve and begin execution"},
						{ID: "reject", Label: "Reject and stop"},
					},
				}},
			})
			if err != nil {
				if errors.Is(err, question.ErrCancelled) {
					response := fantasy.NewTextErrorResponse("Plan review cancelled. Plan mode remains active.")
					response.StopTurn = true
					return response, nil
				}
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(answers) != 1 {
				return fantasy.NewTextErrorResponse("plan review returned an invalid answer"), nil
			}
			answer := answers[0]
			if revision := strings.TrimSpace(answer.FillInText); revision != "" {
				return fantasy.NewTextResponse("Revise the persisted plan using this feedback, then present the complete improved plan and call exit_plan again.\n\n" + revision), nil
			}
			if len(answer.SelectedIDs) != 1 {
				return fantasy.NewTextErrorResponse("select approval, rejection, or provide revision instructions"), nil
			}
			switch answer.SelectedIDs[0] {
			case "approve":
				if err := sessions.SetPlanState(ctx, sessionID, session.ModePlanExecution, plan); err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("failed to approve plan: %w", err)
				}
				return fantasy.NewTextResponse("Plan approved. Begin executing the persisted plan. When implementation and validation are complete, call complete_plan for completion review."), nil
			case "reject":
				response := fantasy.NewTextResponse("Plan rejected. The plan remains in review and execution must not begin.")
				response.StopTurn = true
				return response, nil
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid plan review selection %q", answer.SelectedIDs[0])), nil
			}
		},
	)
}
