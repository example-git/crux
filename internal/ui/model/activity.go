package model

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/proto"
)

const (
	activityRefreshDelay     = 400 * time.Millisecond
	activityIdleRefreshDelay = 5 * time.Second
	activityFetchTimeout     = 3 * time.Second
)

type activityStatusMsg struct {
	status proto.CodebaseIndexStatus
	err    error
}

type activityStatusTickMsg struct{}

func (m *UI) requestActivityRefresh() tea.Cmd {
	if m.activityFetchInFlight || m.com == nil || m.com.Workspace == nil {
		return nil
	}
	m.activityFetchInFlight = true
	workspace := m.com.Workspace
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), activityFetchTimeout)
		defer cancel()
		status, err := workspace.CodebaseIndexStatus(ctx)
		return activityStatusMsg{status: status, err: err}
	}
}

func (m *UI) applyActivityStatus(msg activityStatusMsg) tea.Cmd {
	m.activityFetchInFlight = false
	if msg.err != nil {
		return scheduleActivityRefresh(activityIdleRefreshDelay)
	}
	m.activityStatus = msg.status
	if msg.status.State == "indexing" || msg.status.State == "missing" || msg.status.State == "stale" || msg.status.MemoryActivity != "" {
		return scheduleActivityRefresh(activityRefreshDelay)
	}
	return scheduleActivityRefresh(activityIdleRefreshDelay)
}

func scheduleActivityRefresh(delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return activityStatusTickMsg{}
	})
}

func activityStatusLabel(status proto.CodebaseIndexStatus) string {
	var labels []string
	switch status.State {
	case "indexing":
		label := "indexing"
		switch {
		case status.FilesTotal > 0:
			label = fmt.Sprintf("indexing %d/%d", status.FilesProcessed, status.FilesTotal)
		case status.Stage != "":
			label += " " + status.Stage
		}
		labels = append(labels, label)
	case "ready":
		labels = append(labels, "index ready")
	}
	if status.MemoryActivity != "" {
		labels = append(labels, "memory "+status.MemoryActivity)
	}
	return strings.Join(labels, "  ")
}

func (m *UI) editorAccent() color.Color {
	if m.brand != nil && m.brand.GradB != nil {
		return m.brand.GradB
	}
	return m.com.Styles.Logo.TitleColorB
}

func renderEditorFrameLine(width int, left, label string, accent color.Color) string {
	if width <= 0 {
		return ""
	}
	accentStyle := lipgloss.NewStyle().Foreground(accent)
	status := ""
	if label != "" {
		label = ansi.Truncate(label, max(width-1, 0), "…")
		status = accentStyle.Render(" " + label)
	}
	available := max(width-ansi.StringWidth(status), 0)
	left = ansi.Truncate(left, available, "…")
	fill := max(width-ansi.StringWidth(left)-ansi.StringWidth(status), 0)
	return left + accentStyle.Render(strings.Repeat("─", fill)) + status
}

func paintEditorBody(body string, width int, background color.Color) string {
	if width <= 0 {
		return ""
	}
	style := lipgloss.NewStyle().Background(background).Width(width)
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = style.Render(line)
	}
	return strings.Join(lines, "\n")
}
