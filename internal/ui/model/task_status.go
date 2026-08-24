package model

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	managedtask "github.com/example-git/crux/internal/task"
)

const (
	taskStatusRefreshDelay = time.Second
	taskStatusFetchTimeout = 3 * time.Second
)

type taskStatusMsg struct {
	tasks []managedtask.View
	err   error
}

type taskStatusTickMsg struct{}

func (m *UI) requestTaskStatusRefresh() tea.Cmd {
	if m.taskRefreshInFlight || m.com == nil || m.com.Workspace == nil {
		return nil
	}
	m.taskRefreshInFlight = true
	workspace := m.com.Workspace
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), taskStatusFetchTimeout)
		defer cancel()
		tasks, err := workspace.ListTasks(ctx)
		return taskStatusMsg{tasks: tasks, err: err}
	}
}

func (m *UI) applyTaskStatus(msg taskStatusMsg) tea.Cmd {
	m.taskRefreshInFlight = false
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
