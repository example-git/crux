package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/session"
)

//go:embed update_goal.md
var updateGoalDescription string

const UpdateGoalToolName = "update_goal"

type UpdateGoalParams struct {
	Generation int64  `json:"generation" description:"The active goal generation from the system instructions"`
	Status     string `json:"status" description:"New goal status: active, paused, blocked, or complete"`
	Error      string `json:"error,omitempty" description:"Concrete blocker details; required when status is blocked"`
}

func NewUpdateGoalTool(sessions session.Service) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		UpdateGoalToolName,
		updateGoalDescription,
		func(ctx context.Context, params UpdateGoalParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for updating a goal")
			}
			if params.Generation <= 0 {
				return fantasy.NewTextErrorResponse("goal generation must be greater than zero"), nil
			}
			status := session.GoalStatus(strings.TrimSpace(params.Status))
			switch status {
			case session.GoalStatusActive, session.GoalStatusPaused, session.GoalStatusComplete:
			case session.GoalStatusBlocked:
				if strings.TrimSpace(params.Error) == "" {
					return fantasy.NewTextErrorResponse("blocked goals require a concrete blocker description"), nil
				}
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid goal status %q", params.Status)), nil
			}
			goalError := ""
			if status == session.GoalStatusBlocked {
				goalError = strings.TrimSpace(params.Error)
			}
			updated, err := sessions.SetGoalStatus(ctx, sessionID, params.Generation, status, goalError)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("update goal: %w", err)
			}
			response := fantasy.NewTextResponse(fmt.Sprintf("Goal generation %d is now %s.", updated.Goal.Generation, updated.Goal.Status))
			response.StopTurn = status != session.GoalStatusActive
			return response, nil
		},
	)
}
