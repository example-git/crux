package model

import (
	"context"
	"errors"
	"fmt"
	"testing"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/attachments"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type goalWorkspace struct {
	workspace.Workspace
	sessions      map[string]session.Session
	created       int
	goalMutations int
	agentRuns     int
	goalStops     int
}

func newGoalWorkspace(current ...session.Session) *goalWorkspace {
	result := &goalWorkspace{sessions: make(map[string]session.Session)}
	for _, item := range current {
		result.sessions[item.ID] = item
	}
	return result
}

func (w *goalWorkspace) CreateSession(context.Context, string) (session.Session, error) {
	w.created++
	created := session.Session{ID: "created-session", Title: "New Session"}
	w.sessions[created.ID] = created
	return created, nil
}

func (w *goalWorkspace) GetSession(_ context.Context, sessionID string) (session.Session, error) {
	current, ok := w.sessions[sessionID]
	if !ok {
		return session.Session{}, errors.New("session not found")
	}
	return current, nil
}

func (w *goalWorkspace) SetSessionGoal(_ context.Context, sessionID, objective string) (session.Session, error) {
	w.goalMutations++
	current, err := w.GetSession(context.Background(), sessionID)
	if err != nil {
		return session.Session{}, err
	}
	current.Goal = session.Goal{
		Objective:  objective,
		Status:     session.GoalStatusActive,
		Generation: current.Goal.Generation + 1,
	}
	w.sessions[sessionID] = current
	return current, nil
}

func (w *goalWorkspace) SetSessionGoalStatus(_ context.Context, sessionID string, generation int64, status session.GoalStatus, goalError string) (session.Session, error) {
	current, err := w.GetSession(context.Background(), sessionID)
	if err != nil {
		return session.Session{}, err
	}
	if current.Goal.Generation != generation || current.Goal.Objective == "" {
		return session.Session{}, session.ErrGoalChanged
	}
	current.Goal.Status = status
	current.Goal.Error = goalError
	w.sessions[sessionID] = current
	return current, nil
}

func (w *goalWorkspace) StopSessionGoal(ctx context.Context, sessionID string, generation int64) (session.Session, error) {
	current, err := w.SetSessionGoalStatus(ctx, sessionID, generation, session.GoalStatusPaused, "")
	if err != nil {
		return session.Session{}, err
	}
	current.Goal.Generation++
	w.sessions[sessionID] = current
	w.goalStops++
	return current, nil
}

func (w *goalWorkspace) ClearSessionGoal(_ context.Context, sessionID string) (session.Session, error) {
	current, err := w.GetSession(context.Background(), sessionID)
	if err != nil {
		return session.Session{}, err
	}
	current.Goal = session.Goal{}
	w.sessions[sessionID] = current
	return current, nil
}

func (w *goalWorkspace) AgentRun(context.Context, string, string, ...message.Attachment) error {
	w.agentRuns++
	return nil
}

func (w *goalWorkspace) AuthenticationWorkspaceID() string {
	return "goal-test-workspace"
}

func (w *goalWorkspace) Config() *config.Config {
	return &config.Config{}
}

func TestParseGoalCommand(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		want       goalCommand
		recognized bool
		wantError  string
	}{
		{name: "unrecognized", input: "/goals", recognized: false},
		{name: "status default", input: "/goal", recognized: true, want: goalCommand{kind: goalCommandStatus}},
		{name: "status explicit", input: " /goal status ", recognized: true, want: goalCommand{kind: goalCommandStatus}},
		{name: "set shorthand", input: "/goal ship recovery", recognized: true, want: goalCommand{kind: goalCommandSet, objective: "ship recovery"}},
		{name: "set explicit", input: "/goal set ship recovery", recognized: true, want: goalCommand{kind: goalCommandSet, objective: "ship recovery"}},
		{name: "set requires objective", input: "/goal set", recognized: true, wantError: "goal objective is required"},
		{name: "pause", input: "/goal pause", recognized: true, want: goalCommand{kind: goalCommandPause}},
		{name: "stop", input: "/goal stop", recognized: true, want: goalCommand{kind: goalCommandStop}},
		{name: "resume", input: "/goal resume", recognized: true, want: goalCommand{kind: goalCommandResume}},
		{name: "complete", input: "/goal complete", recognized: true, want: goalCommand{kind: goalCommandComplete}},
		{name: "clear", input: "/goal clear", recognized: true, want: goalCommand{kind: goalCommandClear}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, recognized, err := parseGoalCommand(test.input)
			require.Equal(t, test.recognized, recognized)
			if test.wantError != "" {
				require.EqualError(t, err, test.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, command)
		})
	}
}

func TestRunGoalCommandLifecycle(t *testing.T) {
	ws := newGoalWorkspace()

	created := runGoalCommand(ws, nil, goalCommand{kind: goalCommandSet, objective: "finish the workflow"})().(goalCommandResultMsg)
	require.NoError(t, created.err)
	require.True(t, created.created)
	require.Equal(t, 1, ws.created)
	require.Equal(t, session.Goal{
		Objective:  "finish the workflow",
		Status:     session.GoalStatusActive,
		Generation: 1,
	}, created.session.Goal)

	current := created.session
	for _, test := range []struct {
		kind   goalCommandKind
		status session.GoalStatus
	}{
		{kind: goalCommandPause, status: session.GoalStatusPaused},
		{kind: goalCommandResume, status: session.GoalStatusActive},
		{kind: goalCommandComplete, status: session.GoalStatusComplete},
	} {
		result := runGoalCommand(ws, current, goalCommand{kind: test.kind})().(goalCommandResultMsg)
		require.NoError(t, result.err)
		require.Equal(t, test.status, result.session.Goal.Status)
		require.EqualValues(t, 1, result.session.Goal.Generation)
		current = result.session
	}

	status := runGoalCommand(ws, current, goalCommand{kind: goalCommandStatus})().(goalCommandResultMsg)
	require.NoError(t, status.err)
	require.Contains(t, status.notice, "Goal complete (generation 1)")

	cleared := runGoalCommand(ws, current, goalCommand{kind: goalCommandClear})().(goalCommandResultMsg)
	require.NoError(t, cleared.err)
	require.Empty(t, cleared.session.Goal.Objective)
	require.Equal(t, "Goal cleared.", cleared.notice)

	missing := runGoalCommand(ws, nil, goalCommand{kind: goalCommandStatus})().(goalCommandResultMsg)
	require.NoError(t, missing.err)
	require.Equal(t, "No active goal.", missing.notice)
}

func TestGoalSendPathInterceptsBeforeAttachmentsAndUserMessage(t *testing.T) {
	ws := newGoalWorkspace()
	ui := newTestUI()
	ui.com.Workspace = ws
	ui.keyMap = DefaultKeyMap()
	ui.dialog = dialog.NewOverlay()
	ui.attachments = attachments.New(nil, attachments.Keymap{})
	ui.state = uiLanding
	ui.textarea.SetValue("/goal build interruption recovery")
	ui.promptHistory.index = 2
	ui.promptHistory.draft = "draft"
	ui.attachments.Update(message.Attachment{FileName: "evidence.txt", Content: []byte("evidence")})

	messages := collectCommandMessages(ui.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Len(t, messages, 1)
	result, ok := messages[0].(goalCommandResultMsg)
	require.True(t, ok)
	require.NoError(t, result.err)
	require.True(t, result.created)
	require.Equal(t, "build interruption recovery", result.session.Goal.Objective)
	require.Zero(t, ws.agentRuns)
	require.Empty(t, ui.textarea.Value())
	require.Equal(t, -1, ui.promptHistory.index)
	require.Empty(t, ui.promptHistory.draft)
	require.Len(t, ui.attachments.List(), 1)
	require.Equal(t, "evidence.txt", ui.attachments.List()[0].FileName)

	_, command := ui.Update(result)
	require.NotNil(t, command)
	require.Equal(t, uiChat, ui.state)
	require.Equal(t, result.session.ID, ui.session.ID)
}

func TestGoalCommandIsNotInterceptedInBangMode(t *testing.T) {
	command, recognized, err := parseGoalCommand("/goal shell text")
	require.NoError(t, err)
	require.True(t, recognized)
	require.Equal(t, goalCommandSet, command.kind)

	ws := newGoalWorkspace()
	ui := newTestUI()
	ui.com.Workspace = ws
	ui.keyMap = DefaultKeyMap()
	ui.dialog = dialog.NewOverlay()
	ui.attachments = attachments.New(nil, attachments.Keymap{})
	ui.bangMode = true
	ui.textarea.SetValue("/goal shell text")

	_ = ui.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Zero(t, ws.goalMutations)
	require.Equal(t, 1, ws.created)
	require.Empty(t, ws.sessions["created-session"].Goal.Objective)
	require.Equal(t, "/goal shell text", ui.pendingBangCommand)
}

func TestGoalStatusIncludesBlockedReason(t *testing.T) {
	current := session.Session{Goal: session.Goal{
		Objective:  "finish the work",
		Status:     session.GoalStatusBlocked,
		Generation: 2,
		Error:      "The provider rate limit was reached.",
	}}
	notice := goalNotice(goalCommand{kind: goalCommandStatus}, current)
	require.Contains(t, notice, "Goal blocked")
	require.Contains(t, notice, current.Goal.Error)
	current.Goal.Status = session.GoalStatusActive
	require.NotContains(t, goalNotice(goalCommand{kind: goalCommandStatus}, current), current.Goal.Error)
}

func TestGoalStatusShowsSessionModelBindings(t *testing.T) {
	current := session.Session{Models: []byte(`{"large":{"model":{"provider":"original","model":"main"}},"small":{"model":{"provider":"original","model":"small"}}}`), Goal: session.Goal{Objective: "finish the work", Status: session.GoalStatusActive, Generation: 2}}
	notice := goalNotice(goalCommand{kind: goalCommandStatus}, current)
	require.Contains(t, notice, "Main model: original/main")
	require.Contains(t, notice, "Small model: original/small")
}

func TestInsertCommandUpdatesEditorAndLayout(t *testing.T) {
	ui := newTestUI()
	ui.dialog = dialog.NewOverlay()
	ui.updateLayoutAndSize()
	initialHeight := ui.textarea.Height()

	value := "/goal line one\nline two\nline three\nline four\nline five\nline six"
	_ = ui.handleDialogAction(dialog.ActionInsertCommand{Value: value})
	require.Equal(t, value, ui.textarea.Value())
	require.Equal(t, uiFocusEditor, ui.focus)
	require.Greater(t, ui.textarea.Height(), initialHeight)
}

func TestGoalStopAcceptedWhileRunningAndBetweenTurns(t *testing.T) {
	for _, ready := range []bool{false, true} {
		for _, input := range []string{"escape", "/goal stop"} {
			t.Run(fmt.Sprintf("%s/ready=%t", input, ready), func(t *testing.T) {
				current := session.Session{ID: "goal-session", Goal: session.Goal{Objective: "finish the work", Status: session.GoalStatusActive, Generation: 1}}
				ws := newGoalWorkspace(current)
				ui := newTestUI()
				ui.com.Workspace = ws
				ui.session = &current
				ui.agentReady = ready
				ui.keyMap = DefaultKeyMap()
				ui.dialog = dialog.NewOverlay()
				ui.attachments = attachments.New(nil, attachments.Keymap{})
				ui.textarea.SetValue("keep this draft")
				press := tea.KeyPressMsg{Code: tea.KeyEscape}
				if input != "escape" {
					ui.textarea.SetValue(input)
					press = tea.KeyPressMsg{Code: tea.KeyEnter}
				}

				messages := collectCommandMessages(ui.handleKeyPressMsg(press))
				require.Len(t, messages, 1)
				result, ok := messages[0].(goalCommandResultMsg)
				require.True(t, ok)
				require.NoError(t, result.err)
				require.Equal(t, 1, ws.goalStops)
				require.Equal(t, session.GoalStatusPaused, result.session.Goal.Status)
				require.EqualValues(t, 2, result.session.Goal.Generation)
				require.Equal(t, current.Goal.Objective, result.session.Goal.Objective)
				require.Zero(t, ws.agentRuns)
				if input == "escape" {
					require.Equal(t, "keep this draft", ui.textarea.Value())
				}
			})
		}
	}
}
