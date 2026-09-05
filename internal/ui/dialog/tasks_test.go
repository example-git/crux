package dialog

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type tasksTestWorkspace struct {
	workspace.Workspace
	tasks             []managedtask.View
	notifications     []managedtask.Notification
	output            managedtask.OutputResult
	messages          []message.Message
	stopped           managedtask.View
	continued         managedtask.View
	outputCalls       int
	stopID            string
	continueID        string
	continueSessionID string
	continuePrompt    string
	readID            string
}

func (w *tasksTestWorkspace) WorkingDir() string {
	return "/workspace"
}

func (w *tasksTestWorkspace) ListTasks(context.Context) ([]managedtask.View, error) {
	return w.tasks, nil
}

func (w *tasksTestWorkspace) ListTaskNotifications(context.Context, string, bool) ([]managedtask.Notification, error) {
	return w.notifications, nil
}

func (w *tasksTestWorkspace) TaskOutput(context.Context, string, bool, time.Duration) (managedtask.OutputResult, error) {
	w.outputCalls++
	return w.output, nil
}

func (w *tasksTestWorkspace) ListMessages(context.Context, string) ([]message.Message, error) {
	return w.messages, nil
}

func (w *tasksTestWorkspace) StopTask(_ context.Context, id string) (managedtask.View, error) {
	w.stopID = id
	return w.stopped, nil
}

func (w *tasksTestWorkspace) ContinueTask(_ context.Context, id, parentSessionID, prompt string) (managedtask.View, error) {
	w.continueID = id
	w.continueSessionID = parentSessionID
	w.continuePrompt = prompt
	return w.continued, nil
}

func (w *tasksTestWorkspace) MarkTaskNotificationRead(_ context.Context, id string) (managedtask.Notification, error) {
	w.readID = id
	notification := w.notifications[0]
	notification.ReadAt = time.Now()
	return notification, nil
}

func newTasksTestDialog(workspace *tasksTestWorkspace) *Tasks {
	theme := styles.ThemeForProvider("")
	return NewTasks(&common.Common{Workspace: workspace, Styles: &theme})
}

func runTaskDialogAction(t *testing.T, dialog *Tasks, action Action) {
	t.Helper()
	command, ok := action.(ActionCmd)
	require.True(t, ok)
	message := command.Cmd()
	if batch, ok := message.(tea.BatchMsg); ok {
		for _, cmd := range batch {
			dialog.HandleMsg(cmd())
		}
		return
	}
	dialog.HandleMsg(message)
}

func TestTasksDialogListsNewestFirstAndLoadsUnreadDetail(t *testing.T) {
	older := managedtask.View{ID: "b12345678", Type: managedtask.TypeShell, Description: "shell", Ownership: managedtask.Ownership{ParentSessionID: "parent"}, State: managedtask.State{Status: managedtask.StatusRunning, StartedAt: time.Now().Add(-time.Minute)}, OutputRef: "task-output:b12345678"}
	newer := managedtask.View{ID: "a12345678", Type: managedtask.TypeAgent, Description: "agent", Ownership: managedtask.Ownership{ParentSessionID: "parent"}, State: managedtask.State{Status: managedtask.StatusCompleted, StartedAt: time.Now(), EndedAt: time.Now()}, OutputRef: "session:child", ChildSessionID: "child"}
	workspace := &tasksTestWorkspace{
		tasks:         []managedtask.View{older, newer},
		notifications: []managedtask.Notification{{ID: "notification", TaskID: newer.ID}},
		output:        managedtask.OutputResult{Task: newer, Output: "finished"},
	}
	dialog := newTasksTestDialog(workspace)
	dialog.HandleMsg(dialog.InitialCmd()())
	require.Equal(t, newer.ID, dialog.tasks[0].ID)
	require.Equal(t, taskDialogList, dialog.mode)

	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	runTaskDialogAction(t, dialog, action)
	require.Equal(t, taskDialogDetail, dialog.mode)
	require.Equal(t, "finished", dialog.output.Output)
	require.Equal(t, "notification", workspace.readID)
}

func TestTasksDialogRefreshesRunningShellOutput(t *testing.T) {
	running := managedtask.View{ID: "b12345678", Type: managedtask.TypeShell, Description: "print progress", Command: "printf progress", Ownership: managedtask.Ownership{ParentSessionID: "parent"}, State: managedtask.State{Status: managedtask.StatusRunning}}
	workspace := &tasksTestWorkspace{
		tasks:  []managedtask.View{running},
		output: managedtask.OutputResult{Task: running, Output: "step 1\rstep 2\ncomplete\n"},
	}
	dialog := newTasksTestDialog(workspace)
	runTaskDialogAction(t, dialog, dialog.HandleMsg(dialog.InitialCmd()()))
	require.Equal(t, taskDialogDetail, dialog.mode)
	require.Equal(t, []string{"step 2", "complete"}, terminalOutputLines(dialog.output.Output))

	previousOutput := dialog.output.Output
	action := dialog.HandleMsg(taskDetailTickMsg{taskID: running.ID})
	require.False(t, dialog.loading)
	require.True(t, dialog.refreshing)
	require.Equal(t, previousOutput, dialog.output.Output)
	runTaskDialogAction(t, dialog, action)
	require.False(t, dialog.refreshing)
	require.Equal(t, 2, workspace.outputCalls)
	require.Equal(t, "Shell details", dialog.dialogTitle())
	detail := ansi.Strip(dialog.drawDetail(70, 18))
	require.NotContains(t, detail, "Shell details")
	require.Contains(t, detail, "Command: printf progress")
	require.Contains(t, detail, "Output:")
	require.Contains(t, detail, "step 2")
	require.Contains(t, detail, "Showing 2 lines")
	require.NotContains(t, detail, "$ print progress")
	require.Contains(t, dialog.drawDetail(70, 18), "\x1b[")
}

func TestTasksDialogSeparatesShellOutcomeFromOutput(t *testing.T) {
	exitCode := 0
	completed := managedtask.View{ID: "b12345678", Type: managedtask.TypeShell, Description: "build", Command: "go test ./...", State: managedtask.State{Status: managedtask.StatusCompleted, ExitCode: &exitCode}}
	dialog := newTasksTestDialog(&tasksTestWorkspace{})
	dialog.tasks = []managedtask.View{completed}
	dialog.output = managedtask.OutputResult{Task: completed, Output: "ok\n"}

	detail := dialog.drawDetail(70, 18)
	plain := ansi.Strip(detail)
	require.Contains(t, plain, "Command: go test ./...")
	require.Contains(t, plain, "Exit code: 0\n\nOutput:")
	require.Contains(t, detail, "\x1b[")
}

func TestTasksDialogScrollsFocusedShellOutput(t *testing.T) {
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i+1)
	}
	running := managedtask.View{ID: "b12345678", Type: managedtask.TypeShell, Command: "print-lines", State: managedtask.State{Status: managedtask.StatusRunning}}
	dialog := newTasksTestDialog(&tasksTestWorkspace{})
	dialog.tasks = []managedtask.View{running}
	dialog.mode = taskDialogDetail
	dialog.output = managedtask.OutputResult{Task: running, Output: strings.Join(lines, "\n")}
	dialog.drawDetail(70, 18)

	require.Nil(t, dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}))
	require.True(t, dialog.terminalFocused)
	require.Nil(t, dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyUp}))
	require.Equal(t, 1, dialog.terminalScroll)
	require.Contains(t, ansi.Strip(dialog.drawDetail(70, 18)), "↑/↓ scroll")

	dialog.terminalRect = uv.Rect(0, 0, 20, 20)
	require.Nil(t, dialog.HandleMsg(tea.MouseWheelMsg(tea.Mouse{X: 1, Y: 1, Button: tea.MouseWheelUp})))
	require.Equal(t, 4, dialog.terminalScroll)
	updatedLines := append(append([]string(nil), lines...), "line 21")
	dialog.HandleMsg(taskOutputLoadedMsg{result: managedtask.OutputResult{Task: running, Output: strings.Join(updatedLines, "\n")}})
	require.Equal(t, 5, dialog.terminalScroll)
	require.Nil(t, dialog.HandleMsg(tea.MouseWheelMsg(tea.Mouse{X: 30, Y: 30, Button: tea.MouseWheelUp})))
	require.Equal(t, 5, dialog.terminalScroll)
	require.Nil(t, dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnd}))
	require.Zero(t, dialog.terminalScroll)
}

func TestTasksDialogRendersBackgroundAgentTranscript(t *testing.T) {
	running := managedtask.View{ID: "a12345678", Type: managedtask.TypeAgent, Description: "inspect code", Ownership: managedtask.Ownership{ParentSessionID: "parent"}, State: managedtask.State{Status: managedtask.StatusRunning}, ChildSessionID: "child"}
	workspace := &tasksTestWorkspace{
		tasks:  []managedtask.View{running},
		output: managedtask.OutputResult{Task: running},
		messages: []message.Message{{
			ID:        "assistant-message",
			SessionID: "child",
			Role:      message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Inspecting the coordinator"},
				message.ToolCall{ID: "tool-call", Name: "search", Input: `{"mode":"content","pattern":"background"}`, Finished: true},
			},
		}},
	}
	dialog := newTasksTestDialog(workspace)
	runTaskDialogAction(t, dialog, dialog.HandleMsg(dialog.InitialCmd()()))

	detail := ansi.Strip(dialog.drawDetail(70, 18))
	require.Contains(t, detail, "Agent activity")
	require.Contains(t, detail, "Inspecting the coordinator")
	require.Contains(t, detail, "Search")
}

func TestTasksDialogTruncatesImageDescriptionToOneLine(t *testing.T) {
	dialog := newTasksTestDialog(&tasksTestWorkspace{})
	dialog.tasks = []managedtask.View{{
		ID:          "i12345678",
		Type:        managedtask.TypeImage,
		Description: "generate image: first line\nsecond line " + strings.Repeat("more detail ", 20),
		State:       managedtask.State{Status: managedtask.StatusRunning},
	}}

	const width = 48
	list := ansi.Strip(dialog.drawList(width, 1))
	var contentLines []string
	for line := range strings.SplitSeq(list, "\n") {
		if strings.TrimSpace(line) != "" {
			contentLines = append(contentLines, strings.TrimRight(line, " "))
		}
	}
	require.Len(t, contentLines, 1)
	require.LessOrEqual(t, lipgloss.Width(contentLines[0]), width)
	require.True(t, strings.HasSuffix(contentLines[0], "..."))
	require.NotContains(t, contentLines[0], "…")
}

func TestTasksDialogRendersReadableImageResult(t *testing.T) {
	completed := managedtask.View{
		ID:          "i12345678",
		Type:        managedtask.TypeImage,
		Description: "generate image: a paper fox",
		State:       managedtask.State{Status: managedtask.StatusCompleted, StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now()},
		OutputRef:   "file:/workspace/generated.png",
	}
	dialog := newTasksTestDialog(&tasksTestWorkspace{})
	dialog.tasks = []managedtask.View{completed}
	dialog.mode = taskDialogDetail
	dialog.output = managedtask.OutputResult{Task: completed, Output: `{"success":true,"mode":"generate","outputs":["/workspace/generated.png"],"auth_mode":"codex","model":"gpt-image-2"}`}

	require.Equal(t, "Image details", dialog.dialogTitle())
	detail := ansi.Strip(dialog.drawDetail(70, 18))
	require.Contains(t, detail, "Image generated")
	require.Contains(t, detail, "/workspace/generated.png")
	require.Contains(t, detail, "Account: Codex")
	require.NotContains(t, detail, `{"success"`)
	require.NotContains(t, detail, "Usage:")
}

func TestTasksDialogStopsAndContinuesSupportedTasks(t *testing.T) {
	running := managedtask.View{ID: "b12345678", Type: managedtask.TypeShell, Ownership: managedtask.Ownership{ParentSessionID: "parent"}, State: managedtask.State{Status: managedtask.StatusRunning}}
	workspace := &tasksTestWorkspace{
		tasks:   []managedtask.View{running},
		output:  managedtask.OutputResult{Task: running},
		stopped: managedtask.View{ID: running.ID, Type: managedtask.TypeShell, Ownership: running.Ownership, State: managedtask.State{Status: managedtask.StatusKilled}},
	}
	dialog := newTasksTestDialog(workspace)
	runTaskDialogAction(t, dialog, dialog.HandleMsg(dialog.InitialCmd()()))
	runTaskDialogAction(t, dialog, dialog.HandleMsg(tea.KeyPressMsg{Code: 's'}))
	require.Equal(t, running.ID, workspace.stopID)
	require.Equal(t, managedtask.StatusKilled, dialog.tasks[0].State.Status)

	agentTask := managedtask.View{ID: "a12345678", Type: managedtask.TypeAgent, Ownership: managedtask.Ownership{ParentSessionID: "parent"}, State: managedtask.State{Status: managedtask.StatusCompleted}, ChildSessionID: "child"}
	continued := managedtask.View{ID: "a87654321", Type: managedtask.TypeAgent, Ownership: agentTask.Ownership, State: managedtask.State{Status: managedtask.StatusRunning}, ChildSessionID: "child", ContinuationOf: agentTask.ID}
	workspace.tasks = []managedtask.View{agentTask}
	workspace.output = managedtask.OutputResult{Task: agentTask}
	workspace.continued = continued
	dialog = newTasksTestDialog(workspace)
	runTaskDialogAction(t, dialog, dialog.HandleMsg(dialog.InitialCmd()()))
	require.Nil(t, dialog.HandleMsg(tea.KeyPressMsg{Code: 'c'}))
	dialog.input.SetValue("inspect result")
	runTaskDialogAction(t, dialog, dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Equal(t, agentTask.ID, workspace.continueID)
	require.Equal(t, "parent", workspace.continueSessionID)
	require.Equal(t, "inspect result", workspace.continuePrompt)
	require.Equal(t, continued.ID, dialog.tasks[dialog.selected].ID)
}
