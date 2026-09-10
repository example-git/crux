package model

import (
	"context"
	"fmt"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/util"
)

const (
	taskStatusRefreshDelay = time.Second
	taskStatusFetchTimeout = 3 * time.Second
)

type taskStatusMsg struct {
	tasks           []managedtask.View
	err             error
	sessionID       string
	foregroundCount int
}

type taskStatusTickMsg struct{}

func (m *UI) requestTaskStatusRefresh() tea.Cmd {
	if m.taskRefreshInFlight || m.com == nil || m.com.Workspace == nil {
		return nil
	}
	m.taskRefreshInFlight = true
	workspace := m.com.Workspace
	sessionID := ""
	if m.hasSession() {
		sessionID = m.session.ID
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), taskStatusFetchTimeout)
		defer cancel()
		tasks, err := workspace.ListTasks(ctx)
		msg := taskStatusMsg{tasks: tasks, err: err, sessionID: sessionID}
		if controller, ok := workspace.(foregroundTaskController); ok && sessionID != "" {
			msg.foregroundCount, _ = controller.ForegroundTaskControl(ctx, sessionID, false)
		}
		return msg
	}
}

func (m *UI) applyTaskStatus(msg taskStatusMsg) tea.Cmd {
	m.taskRefreshInFlight = false
	if !m.hasSession() {
		m.foregroundWaitCount = 0
		m.foregroundWaitSessionID = ""
	} else if msg.sessionID == m.session.ID {
		m.foregroundWaitSessionID = msg.sessionID
		m.foregroundWaitCount = msg.foregroundCount
	}
	if msg.err == nil {
		m.runningTaskCount = 0
		for _, task := range msg.tasks {
			if task.State.Status == managedtask.StatusRunning {
				m.runningTaskCount++
			}
		}
	}
	return scheduleTaskStatusRefresh()
}

type foregroundTaskController interface {
	ForegroundTaskControl(context.Context, string, bool) (int, error)
}

func (m *UI) canDetachForeground() bool {
	return m.state == uiChat && m.hasSession() &&
		m.foregroundWaitSessionID == m.session.ID && m.foregroundWaitCount > 0
}

func (m *UI) detachForeground() tea.Cmd {
	controller, ok := m.com.Workspace.(foregroundTaskController)
	if !ok {
		return nil
	}
	sessionID := m.session.ID
	m.foregroundWaitCount = 0
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), taskStatusFetchTimeout)
		defer cancel()
		count, err := controller.ForegroundTaskControl(ctx, sessionID, true)
		if err != nil {
			return util.InfoMsg{Type: util.InfoTypeError, Msg: fmt.Sprintf("Could not background foreground work: %v", err)}
		}
		if count == 0 {
			return util.InfoMsg{Type: util.InfoTypeWarn, Msg: "No foreground work is waiting"}
		}
		return util.InfoMsg{Type: util.InfoTypeSuccess, Msg: "Backgrounding requested"}
	}
}

func scheduleTaskStatusRefresh() tea.Cmd {
	return tea.Tick(taskStatusRefreshDelay, func(time.Time) tea.Msg {
		return taskStatusTickMsg{}
	})
}

func (m *UI) taskStatusTab() string {
	if m.runningTaskCount == 0 {
		return ""
	}
	label := "tasks"
	if m.runningTaskCount == 1 {
		label = "task"
	}
	style := m.com.Styles.Editor.TasksTab.
		Foreground(m.com.Styles.Editor.Background).
		Background(m.editorAccent())
	return style.Render(fmt.Sprintf(" ctrl+↓ %d %s ", m.runningTaskCount, label))
}
