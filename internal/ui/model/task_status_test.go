package model

import (
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	managedtask "github.com/example-git/crux/internal/task"
)

func TestTaskStatusCountsOnlyRunningTasks(t *testing.T) {
	ui := newTestUI()

	cmd := ui.applyTaskStatus(taskStatusMsg{tasks: []managedtask.View{
		{ID: "b12345678", State: managedtask.State{Status: managedtask.StatusRunning}},
		{ID: "a12345678", State: managedtask.State{Status: managedtask.StatusRunning}},
		{ID: "b87654321", State: managedtask.State{Status: managedtask.StatusPending}},
		{ID: "a87654321", State: managedtask.State{Status: managedtask.StatusCompleted}},
	}})

	require.NotNil(t, cmd)
	require.Equal(t, 2, ui.runningTaskCount)
	require.Equal(t, " ctrl+↓ 2 tasks ", ansi.Strip(ui.taskStatusTab()))
}

func TestTaskStatusTabIsIntegratedIntoBottomEditorOutline(t *testing.T) {
	ui := newTestUI()
	ui.runningTaskCount = 1

	view := ui.renderEditorView(32)
	lines := strings.Split(view, "\n")
	bottom := lines[len(lines)-1]

	require.Equal(t, 32, ansi.StringWidth(bottom))
	require.Equal(t, " ctrl+↓ 1 task "+strings.Repeat("─", 17), ansi.Strip(bottom))
	require.Contains(t, bottom, "\x1b[")
}

func TestTaskStatusTabUsesProviderGradientB(t *testing.T) {
	ui := newTestUI()
	accent := color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xff}
	ui.brand = &providerBrand{GradB: accent}
	ui.runningTaskCount = 1

	expected := ui.com.Styles.Editor.TasksTab.
		Foreground(ui.com.Styles.Editor.Background).
		Background(accent).
		Render(" ctrl+↓ 1 task ")

	require.Equal(t, expected, ui.taskStatusTab())
}

func TestTaskStatusErrorRetainsLastKnownCount(t *testing.T) {
	ui := newTestUI()
	ui.runningTaskCount = 2

	cmd := ui.applyTaskStatus(taskStatusMsg{err: assertTaskStatusError{}})

	require.NotNil(t, cmd)
	require.Equal(t, 2, ui.runningTaskCount)
}

type assertTaskStatusError struct{}

func (assertTaskStatusError) Error() string {
	return "task status unavailable"
}
