package tools

import (
	"context"
	"encoding/json"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

type planToolResult struct {
	response fantasy.ToolResponse
	err      error
}

func TestPlanToolsLifecycle(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := session.NewService(db.New(conn), conn)
	questions := question.NewService()
	created, err := sessions.Create(t.Context(), "plan test")
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, created.ID)

	enterResponse, err := NewEnterPlanTool(sessions).Run(ctx, fantasy.ToolCall{
		ID:    "enter",
		Name:  EnterPlanToolName,
		Input: `{}`,
	})
	require.NoError(t, err)
	require.False(t, enterResponse.IsError)
	current, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.ModePlan, current.Mode)
	require.Empty(t, current.Plan)

	events := questions.Subscribe(t.Context())
	exitTool := NewExitPlanTool(sessions, questions)
	revision := runPlanReview(t, ctx, exitTool, ExitPlanToolName, ExitPlanParams{Plan: "Inspect, then implement"}, events, questions, question.Answer{FillInText: "Add rollback validation"})
	require.False(t, revision.IsError)
	require.False(t, revision.StopTurn)
	require.Contains(t, revision.Content, "Add rollback validation")
	current, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.ModePlanRevision, current.Mode)
	require.Equal(t, "Inspect, then implement", current.Plan)

	approvedPlan := "Inspect, implement, then validate rollback"
	approved := runPlanReview(t, ctx, exitTool, ExitPlanToolName, ExitPlanParams{Plan: approvedPlan}, events, questions, question.Answer{SelectedIDs: []string{"approve"}})
	require.False(t, approved.IsError)
	require.False(t, approved.StopTurn)
	require.Contains(t, approved.Content, "Begin executing")
	current, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.ModePlanExecution, current.Mode)
	require.Equal(t, approvedPlan, current.Plan)

	completeTool := NewCompletePlanTool(sessions, questions)
	continued := runPlanReview(t, ctx, completeTool, CompletePlanToolName, CompletePlanParams{Summary: "Implemented and tested"}, events, questions, question.Answer{SelectedIDs: []string{"continue"}})
	require.False(t, continued.IsError)
	require.Contains(t, continued.Content, "Continue executing")
	current, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.ModePlanExecution, current.Mode)
	require.Equal(t, approvedPlan, current.Plan)

	feedback := runPlanReview(t, ctx, completeTool, CompletePlanToolName, CompletePlanParams{Summary: "Implemented and tested"}, events, questions, question.Answer{FillInText: "Add the missing boundary test"})
	require.False(t, feedback.IsError)
	require.Contains(t, feedback.Content, "Add the missing boundary test")
	current, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.ModePlanExecution, current.Mode)

	completed := runPlanReview(t, ctx, completeTool, CompletePlanToolName, CompletePlanParams{Summary: "Implemented with boundary coverage"}, events, questions, question.Answer{SelectedIDs: []string{"approve"}})
	require.False(t, completed.IsError)
	require.True(t, completed.StopTurn)
	require.Contains(t, completed.Content, "normal instructions are restored")
	current, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.ModeDefault, current.Mode)
	require.Empty(t, current.Plan)
}

func TestExitPlanRejectionRetainsPlanReview(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := session.NewService(db.New(conn), conn)
	questions := question.NewService()
	created, err := sessions.Create(t.Context(), "plan test")
	require.NoError(t, err)
	require.NoError(t, sessions.SetMode(t.Context(), created.ID, session.ModePlan))
	ctx := context.WithValue(t.Context(), SessionIDContextKey, created.ID)

	rejected := runPlanReview(t, ctx, NewExitPlanTool(sessions, questions), ExitPlanToolName, ExitPlanParams{Plan: "Inspect, then implement"}, questions.Subscribe(t.Context()), questions, question.Answer{SelectedIDs: []string{"reject"}})
	require.False(t, rejected.IsError)
	require.True(t, rejected.StopTurn)
	current, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.ModePlanRevision, current.Mode)
	require.Equal(t, "Inspect, then implement", current.Plan)
}

func TestPlanToolsRejectInvalidStateAndInput(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := session.NewService(db.New(conn), conn)
	created, err := sessions.Create(t.Context(), "plan test")
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, created.ID)
	exitTool := NewExitPlanTool(sessions, question.NewService())

	response, err := runPlanTool(ctx, exitTool, ExitPlanToolName, ExitPlanParams{Plan: "Inspect, then implement"})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "drafting or revision mode is not active")

	require.NoError(t, sessions.SetMode(t.Context(), created.ID, session.ModePlan))
	response, err = runPlanTool(ctx, exitTool, ExitPlanToolName, ExitPlanParams{})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "plan is required")

	completeTool := NewCompletePlanTool(sessions, question.NewService())
	response, err = runPlanTool(ctx, completeTool, CompletePlanToolName, CompletePlanParams{Summary: "Done"})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "approved plan is not being executed")

	require.NoError(t, sessions.SetPlanState(t.Context(), created.ID, session.ModePlanExecution, "Implement"))
	response, err = runPlanTool(ctx, completeTool, CompletePlanToolName, CompletePlanParams{})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "completion summary is required")
}

func runPlanReview(
	t *testing.T,
	ctx context.Context,
	tool fantasy.AgentTool,
	name string,
	params any,
	events <-chan pubsub.Event[question.Request],
	questions question.Service,
	answer question.Answer,
) fantasy.ToolResponse {
	t.Helper()
	result := make(chan planToolResult, 1)
	go func() {
		response, err := runPlanTool(ctx, tool, name, params)
		result <- planToolResult{response: response, err: err}
	}()

	event := <-events
	request := event.Payload
	require.Len(t, request.Questions, 1)
	answer.QuestionID = request.Questions[0].ID
	require.True(t, questions.Answer([]question.Answer{answer}))
	outcome := <-result
	require.NoError(t, outcome.err)
	return outcome.response
}

func runPlanTool(ctx context.Context, tool fantasy.AgentTool, name string, params any) (fantasy.ToolResponse, error) {
	input, err := json.Marshal(params)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	return tool.Run(ctx, fantasy.ToolCall{ID: "review", Name: name, Input: string(input)})
}
