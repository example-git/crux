package model

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/workspace"
)

type goalCommandKind string

const (
	goalCommandSet      goalCommandKind = "set"
	goalCommandStatus   goalCommandKind = "status"
	goalCommandPause    goalCommandKind = "pause"
	goalCommandStop     goalCommandKind = "stop"
	goalCommandResume   goalCommandKind = "resume"
	goalCommandComplete goalCommandKind = "complete"
	goalCommandClear    goalCommandKind = "clear"
)

type goalCommand struct {
	kind      goalCommandKind
	objective string
}

type goalCommandResultMsg struct {
	session *session.Session
	created bool
	notice  string
	err     error
}

func parseGoalCommand(input string) (goalCommand, bool, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed != "/goal" && !strings.HasPrefix(trimmed, "/goal ") {
		return goalCommand{}, false, nil
	}
	arguments := strings.TrimSpace(strings.TrimPrefix(trimmed, "/goal"))
	if arguments == "" || arguments == "status" {
		return goalCommand{kind: goalCommandStatus}, true, nil
	}
	switch arguments {
	case "pause":
		return goalCommand{kind: goalCommandPause}, true, nil
	case "stop":
		return goalCommand{kind: goalCommandStop}, true, nil
	case "resume":
		return goalCommand{kind: goalCommandResume}, true, nil
	case "complete":
		return goalCommand{kind: goalCommandComplete}, true, nil
	case "clear":
		return goalCommand{kind: goalCommandClear}, true, nil
	}
	if arguments == "set" {
		return goalCommand{}, true, fmt.Errorf("goal objective is required")
	}
	if after, ok := strings.CutPrefix(arguments, "set "); ok {
		arguments = strings.TrimSpace(after)
		if arguments == "" {
			return goalCommand{}, true, fmt.Errorf("goal objective is required")
		}
	}
	return goalCommand{kind: goalCommandSet, objective: arguments}, true, nil
}

func runGoalCommand(workspace workspace.Workspace, current *session.Session, command goalCommand) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		var selected session.Session
		created := false
		if current != nil {
			loaded, err := workspace.GetSession(ctx, current.ID)
			if err != nil {
				return goalCommandResultMsg{err: err}
			}
			selected = loaded
		} else if command.kind == goalCommandSet {
			newSession, err := workspace.CreateSession(ctx, "New Session")
			if err != nil {
				return goalCommandResultMsg{err: err}
			}
			selected = newSession
			created = true
		} else {
			return goalCommandResultMsg{notice: "No active goal."}
		}

		var updated session.Session
		var err error
		switch command.kind {
		case goalCommandSet:
			updated, err = workspace.SetSessionGoal(ctx, selected.ID, command.objective)
		case goalCommandStatus:
			updated = selected
		case goalCommandPause, goalCommandResume, goalCommandComplete:
			if selected.Goal.Objective == "" {
				return goalCommandResultMsg{session: &selected, created: created, notice: "No active goal."}
			}
			status := session.GoalStatusPaused
			switch command.kind {
			case goalCommandResume:
				status = session.GoalStatusActive
			case goalCommandComplete:
				status = session.GoalStatusComplete
			}
			updated, err = workspace.SetSessionGoalStatus(ctx, selected.ID, selected.Goal.Generation, status, "")
		case goalCommandStop:
			if selected.Goal.Objective == "" {
				return goalCommandResultMsg{session: &selected, created: created, notice: "No active goal."}
			}
			updated, err = workspace.StopSessionGoal(ctx, selected.ID, selected.Goal.Generation)
		case goalCommandClear:
			if selected.Goal.Objective == "" {
				return goalCommandResultMsg{session: &selected, created: created, notice: "No active goal."}
			}
			updated, err = workspace.ClearSessionGoal(ctx, selected.ID)
		default:
			err = fmt.Errorf("unsupported goal command %q", command.kind)
		}
		if err != nil {
			return goalCommandResultMsg{err: err}
		}
		return goalCommandResultMsg{session: &updated, created: created, notice: goalNotice(command, updated)}
	}
}

func goalNotice(command goalCommand, current session.Session) string {
	if command.kind == goalCommandStop {
		return "Goal stopped; use /goal resume to continue."
	}
	if command.kind == goalCommandClear {
		return "Goal cleared."
	}
	if current.Goal.Objective == "" {
		return "No active goal."
	}
	return fmt.Sprintf("Goal %s (generation %d): %s", current.Goal.Status, current.Goal.Generation, current.Goal.Objective)
}
