package dialog

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

type blockedPanelWorkspace struct {
	*tasksTestWorkspace
	started chan struct{}
	release chan struct{}
}

func (w *blockedPanelWorkspace) TaskOutput(ctx context.Context, id string, _ bool, _ time.Duration) (managedtask.OutputResult, error) {
	close(w.started)
	<-w.release
	return managedtask.OutputResult{Task: w.tasks[0], Output: "late output"}, nil
}

func TestTaskPanelRestartKeyClickAndStaleReplies(t *testing.T) {
	for _, width := range []int{40, 65, 120, 160} {
		for _, click := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/click=%v", width, click), func(t *testing.T) {
				theme := styles.ThemeForProvider("")
				task := managedtask.View{ID: "b12345678", Type: managedtask.TypeShell, OutputRef: "task-output:b12345678", State: managedtask.State{Status: managedtask.StatusRunning}}
				d := NewTasksPanel(&common.Common{Workspace: &tasksTestWorkspace{}, Styles: &theme})
				defer d.ClosePanel()
				d.tasks = []managedtask.View{task}
				d.mode = taskDialogDetail
				d.loading = false
				d.output = managedtask.OutputResult{Task: task, Output: "old output"}
				d.terminalScroll = 3
				oldRequest := d.outputRequest
				var action Action
				if click {
					screen := uv.NewScreenBuffer(width, 12)
					d.DrawPanel(screen, image.Rect(0, 0, width, 12), theme.Logo.TitleColorB)
					for _, button := range d.panelButtons {
						if button.key.Code == 'R' {
							action = d.HandleMsg(tea.MouseClickMsg{Button: tea.MouseLeft, X: button.rect.Min.X, Y: button.rect.Min.Y})
							break
						}
					}
				} else {
					action = d.HandleMsg(tea.KeyPressMsg{Code: 'R', Text: "R"})
				}
				command, ok := action.(ActionCmd)
				require.True(t, ok, "restart must return asynchronous command")
				require.True(t, d.loading)
				require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'r', Text: "r"}))
				d.HandleMsg(taskOutputLoadedMsg{request: oldRequest, result: managedtask.OutputResult{Task: task, Output: "stale"}})
				require.Equal(t, "old output", d.output.Output)
				reply := command.Cmd()
				d.HandleMsg(reply)
				require.Empty(t, d.output.Output)
				require.Zero(t, d.terminalScroll)
				selected, ok := d.selectedTask()
				require.True(t, ok)
				require.Equal(t, task.ID, selected.ID)
				require.Equal(t, "task-output:b87654321", selected.OutputRef)
				d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
				d.HandleMsg(reply)
				require.Equal(t, taskDialogList, d.mode)
			})
		}
	}
}

func TestTaskPanelListFrameClearSurfaceAndRowNavigation(t *testing.T) {
	for _, width := range []int{40, 65, 120, 160} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			theme := styles.ThemeForProvider("")
			require.Equal(t, color.RGBA{A: 255}, color.RGBAModel.Convert(theme.TaskPanel.OutputBackground))
			tasks := []managedtask.View{
				{ID: "shell-one", Type: managedtask.TypeShell, Description: "Build application", State: managedtask.State{Status: managedtask.StatusRunning}},
				{ID: "shell-two", Type: managedtask.TypeShell, Description: "Check results", State: managedtask.State{Status: managedtask.StatusCompleted}},
			}
			d := NewTasksPanel(&common.Common{Workspace: &tasksTestWorkspace{}, Styles: &theme})
			defer d.ClosePanel()
			d.loading = false
			d.tasks = tasks
			screen := uv.NewScreenBuffer(width, 10)
			d.DrawPanel(screen, image.Rect(0, 0, width, 10), theme.Logo.TitleColorB)
			require.Equal(t, "╭", screen.CellAt(0, 0).Content)
			require.Equal(t, "╮", screen.CellAt(width-1, 0).Content)
			for x := 1; x < width-1; x++ {
				require.Equal(t, "─", screen.CellAt(x, 0).Content, "thin top border cell %d", x)
				require.Equal(t, "▄", screen.CellAt(x, 2).Content, "thick top border cell %d", x)
			}
			require.Equal(t, "╰", screen.CellAt(0, 8).Content)
			require.Equal(t, "╯", screen.CellAt(width-1, 8).Content)
			for y := 3; y < 8; y++ {
				require.Equal(t, "▐", screen.CellAt(0, y).Content)
				require.Equal(t, "▌", screen.CellAt(width-1, y).Content)
			}
			for _, y := range []int{0, 1, 3, 9} {
				for x := range width {
					require.Equal(t, color.RGBAModel.Convert(theme.Background), color.RGBAModel.Convert(screen.CellAt(x, y).Style.Bg), "clear cell %d,%d", x, y)
				}
			}
			require.Equal(t, "›", screen.CellAt(2, 4).Content)
			require.Equal(t, color.RGBAModel.Convert(theme.TaskPanel.ControlsBackground), color.RGBAModel.Convert(screen.CellAt(2, 4).Style.Bg))
			var second strings.Builder
			for x := 1; x < width-1; x++ {
				cell := screen.CellAt(x, 5)
				require.Equal(t, color.RGBAModel.Convert(theme.Background), color.RGBAModel.Convert(cell.Style.Bg))
				second.WriteString(cell.Content)
			}
			require.Contains(t, second.String(), "Check results")
			require.Contains(t, second.String(), "completed")
			require.IsType(t, ActionCmd{}, d.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: 4, Y: 5, Button: tea.MouseLeft})))
			require.Equal(t, 1, d.selected)
			require.Equal(t, taskDialogDetail, d.mode)
			d.DrawPanel(screen, image.Rect(0, 0, width, 10), theme.Logo.TitleColorB)
			require.Equal(t, color.RGBA{A: 255}, color.RGBAModel.Convert(screen.CellAt(2, 3).Style.Bg))
			d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
			d.tasks = nil
			d.DrawPanel(screen, image.Rect(0, 0, width, 10), theme.Logo.TitleColorB)
			require.Equal(t, "│", screen.CellAt(0, 1).Content)
			require.Equal(t, "│", screen.CellAt(width-1, 1).Content)
			for y := 3; y < 8; y++ {
				for x := 1; x < width-1; x++ {
					require.Equal(t, color.RGBAModel.Convert(theme.Background), color.RGBAModel.Convert(screen.CellAt(x, y).Style.Bg))
				}
			}
		})
	}
}

func TestTaskPanelRetriesOutputErrorsAndRejectsClosedReplies(t *testing.T) {
	task := managedtask.View{ID: "shell-one", Type: managedtask.TypeShell, State: managedtask.State{Status: managedtask.StatusRunning}}
	theme := styles.ThemeForProvider("")
	ws := &tasksTestWorkspace{tasks: []managedtask.View{task}, output: managedtask.OutputResult{Task: task, Output: "recovered output"}}
	d := NewTasksPanel(&common.Common{Workspace: ws, Styles: &theme})
	defer d.ClosePanel()
	d.HandleMsg(d.InitialCmd()())
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	action := d.HandleMsg(taskOutputLoadedMsg{request: d.outputRequest, err: fmt.Errorf("temporary read error")})
	require.Error(t, d.actionErr)
	require.NotNil(t, action.(ActionCmd).Cmd)
	retry := d.HandleMsg(taskDetailTickMsg{taskID: task.ID}).(ActionCmd)
	d.HandleMsg(retry.Cmd())
	require.NoError(t, d.actionErr)
	require.Contains(t, strings.Join(d.panelLines, "\n"), "recovered output")
	d.ClosePanel()
	d.HandleMsg(taskOutputLoadedMsg{request: d.outputRequest, result: managedtask.OutputResult{Task: task}, lines: []string{"late"}})
	require.NotContains(t, d.panelLines, "late")
}

func TestTaskPanelNotificationSelectionAndCompletion(t *testing.T) {
	task := managedtask.View{ID: "shell-one", Type: managedtask.TypeShell, State: managedtask.State{Status: managedtask.StatusRunning}}
	theme := styles.ThemeForProvider("")
	ws := &tasksTestWorkspace{tasks: []managedtask.View{task}}
	d := NewTasksPanel(&common.Common{Workspace: ws, Styles: &theme})
	defer d.ClosePanel()
	d.tasks = ws.tasks
	d.mode = taskDialogDetail
	cmd := d.loadPanelNotifications(task)
	d.actionRequest++
	d.tasks[0].State.Status = managedtask.StatusCompleted
	final := d.HandleMsg(cmd()).(ActionCmd)
	require.NotNil(t, final.Cmd)
	d.HandleMsg(final.Cmd())
	require.False(t, d.panelNotificationsLoading)
	require.Equal(t, task.ID, d.panelNotificationsTaskID)
	d.loadSelectedOutput(true)
	d.HandleMsg(taskPanelNotificationsMsg{request: d.panelNotificationsRequest - 1, taskID: task.ID, terminal: true, err: fmt.Errorf("stale")})
	require.NoError(t, d.panelNotificationsErr)
}

func TestTaskPanelRollingOutputKeepsEarlierPosition(t *testing.T) {
	task := managedtask.View{ID: "shell-one", Type: managedtask.TypeShell, State: managedtask.State{Status: managedtask.StatusRunning}}
	theme := styles.ThemeForProvider("")
	d := NewTasksPanel(&common.Common{Workspace: &tasksTestWorkspace{}, Styles: &theme})
	defer d.ClosePanel()
	d.tasks = []managedtask.View{task}
	d.mode = taskDialogDetail
	d.output = managedtask.OutputResult{Task: task, Output: "a\nb\nc\n", NextOffset: 100}
	d.panelLines = []string{"a", "b", "c"}
	d.terminalScroll = 1
	result := managedtask.OutputResult{Task: task, Output: "b\nc\nd\n", NextOffset: 102, OutputTruncated: true}
	d.HandleMsg(taskOutputLoadedMsg{result: result, lines: []string{"b", "c", "d"}})
	require.Equal(t, 2, d.terminalScroll)
	require.Equal(t, 1, d.terminalUnseenLines)
	require.Contains(t, d.panelOutputPosition(100), "Output truncated")
}

func TestTaskPanelNavigationWhileOutputBlocked(t *testing.T) {
	task := managedtask.View{ID: "shell-one", Type: managedtask.TypeShell, State: managedtask.State{Status: managedtask.StatusCompleted}}
	ws := &blockedPanelWorkspace{tasksTestWorkspace: &tasksTestWorkspace{tasks: []managedtask.View{task}}, started: make(chan struct{}), release: make(chan struct{})}
	theme := styles.ThemeForProvider("")
	d := NewTasksPanel(&common.Common{Workspace: ws, Styles: &theme})
	defer d.ClosePanel()
	d.HandleMsg(d.InitialCmd()())
	require.Equal(t, taskDialogList, d.mode)
	cmd := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionCmd).Cmd
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	select {
	case <-ws.started:
	case <-time.After(time.Second):
		t.Fatal("output command did not start")
	}
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, taskDialogList, d.mode)
	require.IsType(t, ActionClose{}, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape}))
	close(ws.release)
	d.HandleMsg(<-result)
	require.Empty(t, d.panelLines)
	require.Equal(t, taskDialogList, d.mode)
}

func TestTaskPanelCachedOutputScrollingAndClick(t *testing.T) {
	for _, width := range []int{40, 80, 160} {
		task := managedtask.View{ID: "shell-one", Type: managedtask.TypeShell, FinalOutput: strings.Repeat("output line\n", 10000), State: managedtask.State{Status: managedtask.StatusCompleted}}
		theme := styles.ThemeForProvider("")
		d := NewPreviewTasks(&common.Common{Styles: &theme}, []managedtask.View{task}, true)
		defer d.ClosePanel()
		screen := uv.NewScreenBuffer(width, 15)
		d.DrawPanel(screen, image.Rect(0, 4, width, 15), theme.Logo.TitleColorB)
		require.LessOrEqual(t, d.terminalViewportHeight, 9)
		d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyHome})
		require.Greater(t, d.terminalScroll, 9900)
		d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnd})
		require.Zero(t, d.terminalScroll)
		d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
		d.DrawPanel(screen, image.Rect(0, 4, width, 15), theme.Logo.TitleColorB)
		_, ok := d.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: 2, Y: 8, Button: tea.MouseLeft})).(ActionCmd)
		require.True(t, ok)
		require.Equal(t, taskDialogDetail, d.mode)
	}
}

func TestTaskPanelDetailTerminalFrame(t *testing.T) {
	for _, width := range []int{65, 120, 160} {
		task := managedtask.View{ID: "shell-one", Type: managedtask.TypeShell, Description: "Task metadata", Command: "printf hello", FinalOutput: "hello", State: managedtask.State{Status: managedtask.StatusRunning}}
		theme := styles.ThemeForProvider("")
		d := NewPreviewTasks(&common.Common{Styles: &theme}, []managedtask.View{task}, true)
		screen := uv.NewScreenBuffer(width, 15)
		d.DrawPanel(screen, image.Rect(0, 4, width, 15), theme.Logo.TitleColorB)
		for _, y := range []int{4, 13} {
			for x := 1; x < width-1; x++ {
				cell := screen.CellAt(x, y)
				require.Equal(t, color.RGBAModel.Convert(theme.PanelBackground), color.RGBAModel.Convert(cell.Style.Bg))
				if y == 4 || cell.Content == "▀" {
					edge := "▀"
					if y == 4 {
						edge = "▄"
					}
					require.Equal(t, edge, cell.Content)
					require.Equal(t, color.RGBAModel.Convert(theme.Logo.TitleColorB), color.RGBAModel.Convert(cell.Style.Fg))
				}
			}
		}
		for point, glyph := range map[image.Point]string{
			image.Pt(0, 4): "╭", image.Pt(width-1, 4): "╮",
			image.Pt(0, 13): "╰", image.Pt(width-1, 13): "╯",
		} {
			cell := screen.CellAt(point.X, point.Y)
			require.Equal(t, glyph, cell.Content)
			require.Equal(t, color.RGBAModel.Convert(theme.PanelBackground), color.RGBAModel.Convert(cell.Style.Bg))
		}
		for y := 5; y < 13; y++ {
			for _, x := range []int{0, width - 1} {
				edge := "▐"
				if x == width-1 {
					edge = "▌"
				}
				require.Equal(t, edge, screen.CellAt(x, y).Content)
				require.Equal(t, color.RGBAModel.Convert(theme.PanelBackground), color.RGBAModel.Convert(screen.CellAt(x, y).Style.Bg))
			}
			for x := 1; x < width-1; x++ {
				require.Equal(t, color.RGBAModel.Convert(theme.TaskPanel.OutputBackground), color.RGBAModel.Convert(screen.CellAt(x, y).Style.Bg), "body cell %d,%d", x, y)
			}
		}
		require.Equal(t, 2, d.terminalRect.Min.X)
		require.Equal(t, width-2, d.terminalRect.Max.X)
		for y := 5; y < 13; y++ {
			require.Equal(t, " ", screen.CellAt(1, y).Content)
			require.Equal(t, " ", screen.CellAt(width-2, y).Content)
		}
		require.Equal(t, "h", screen.CellAt(2, 6).Content)
		require.Equal(t, 6, d.terminalRect.Min.Y)
		require.Equal(t, 12, d.terminalRect.Max.Y)
		for _, y := range []int{5, 12} {
			for x := 1; x < width-1; x++ {
				require.Equal(t, " ", screen.CellAt(x, y).Content)
			}
		}
		require.NotContains(t, d.panelLines, "$ printf hello")
		require.Contains(t, d.panelLines, "hello")
		require.NotContains(t, d.panelLines, "Task metadata")
		var footer strings.Builder
		for x := 0; x < width; x++ {
			footer.WriteString(screen.CellAt(x, 14).Content)
		}
		for x := 0; x < width; x++ {
			require.Equal(t, color.RGBAModel.Convert(theme.TaskPanel.ControlsBackground), color.RGBAModel.Convert(screen.CellAt(x, 14).Style.Bg))
		}
		for _, label := range []string{"back", "stop", "refresh", "scroll"} {
			require.Contains(t, footer.String(), label)
		}
		d.ClosePanel()
	}
}

func TestTaskPanelIgnoresLateActionsAndCoalescesListLoads(t *testing.T) {
	theme := styles.ThemeForProvider("")
	d := NewTasksPanel(&common.Common{Workspace: &tasksTestWorkspace{}, Styles: &theme})
	defer d.ClosePanel()
	require.NotNil(t, d.InitialCmd())
	require.Nil(t, d.loadCmd())
	d.mode = taskDialogDetail
	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	d.HandleMsg(taskContinuedMsg{request: 0, task: managedtask.View{ID: "late"}})
	d.HandleMsg(taskStoppedMsg{request: 0, task: managedtask.View{ID: "late"}})
	require.Equal(t, taskDialogList, d.mode)
	require.Empty(t, d.tasks)
}

func TestTaskPanelLayeredInfo(t *testing.T) {
	for _, width := range []int{40, 65, 120, 160} {
		task := managedtask.View{ID: "shell-one", Type: managedtask.TypeShell, Description: "Review layout stage 1", Command: "go test ./fixture", State: managedtask.State{Status: managedtask.StatusCompleted}}
		theme := styles.ThemeForProvider("")
		d := NewPreviewTasks(&common.Common{Styles: &theme}, []managedtask.View{task}, true)
		screen := uv.NewScreenBuffer(width, 4)
		uv.NewStyledString(d.RenderPanelInfo(width, theme.Logo.TitleColorB)).Draw(screen, image.Rect(0, 0, width, 4))
		var title strings.Builder
		for x := 0; x < width; x++ {
			title.WriteString(screen.CellAt(x, 1).Content)
			for y := 0; y < 4; y++ {
				require.NotNil(t, screen.CellAt(x, y).Style.Bg, "header cell %d,%d width=%d", x, y, width)
				require.Equal(t, color.RGBAModel.Convert(theme.PanelBackground), color.RGBAModel.Convert(screen.CellAt(x, y).Style.Bg))
			}
		}
		require.Contains(t, title.String(), "Review layout")
		require.Contains(t, title.String(), "✓ completed")
		require.NotContains(t, title.String(), task.ID)
		require.Equal(t, color.RGBAModel.Convert(theme.TaskPanel.Title.GetForeground()), color.RGBAModel.Convert(screen.CellAt(2, 1).Style.Fg))
		require.Equal(t, color.RGBAModel.Convert(theme.TaskPanel.Metadata.GetForeground()), color.RGBAModel.Convert(screen.CellAt(2, 2).Style.Fg))
		require.Equal(t, color.RGBAModel.Convert(theme.Logo.TitleColorB), color.RGBAModel.Convert(screen.CellAt(2, 3).Style.Fg))
		require.Empty(t, d.panelLines)
		d.ClosePanel()
	}
}

func TestTaskPanelOutputFollowingAndLatest(t *testing.T) {
	for _, width := range []int{40, 65, 120} {
		task := managedtask.View{ID: "shell-one", Type: managedtask.TypeShell, FinalOutput: strings.Repeat("line\n", 30), State: managedtask.State{Status: managedtask.StatusRunning}}
		theme := styles.ThemeForProvider("")
		d := NewPreviewTasks(&common.Common{Styles: &theme}, []managedtask.View{task}, true)
		screen := uv.NewScreenBuffer(width, 12)
		draw := func() { d.DrawPanel(screen, image.Rect(0, 0, width, 12), theme.Logo.TitleColorB) }
		draw()
		require.Equal(t, "Following output ↓", d.panelOutputPosition(width-4))
		d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyUp})
		require.Equal(t, 1, d.terminalScroll)
		oldEnd := len(d.panelLines) - d.terminalScroll
		result := managedtask.OutputResult{Task: task, Output: task.FinalOutput + "new one\nnew two\n"}
		lines, maxWidth := prepareTaskPanelOutput(result, nil)
		d.HandleMsg(taskOutputLoadedMsg{request: d.outputRequest, result: result, lines: lines, maxWidth: maxWidth})
		draw()
		require.Equal(t, oldEnd, len(d.panelLines)-d.terminalScroll)
		require.Equal(t, 2, d.terminalUnseenLines)
		require.Contains(t, d.panelOutputPosition(width-4), "2 new lines")
		require.Contains(t, d.panelOutputPosition(width-4), "End latest")
		d.HandleMsg(taskOutputLoadedMsg{request: d.outputRequest, result: result, lines: lines, maxWidth: maxWidth})
		require.Equal(t, 2, d.terminalUnseenLines)
		clicked := false
		for _, button := range d.panelButtons {
			if button.key.Code == tea.KeyEnd {
				d.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: button.rect.Min.X, Y: button.rect.Min.Y, Button: tea.MouseLeft}))
				clicked = true
				break
			}
		}
		require.True(t, clicked)
		require.Zero(t, d.terminalScroll)
		require.Zero(t, d.terminalUnseenLines)
		result.Output += "following\n"
		lines, maxWidth = prepareTaskPanelOutput(result, nil)
		d.HandleMsg(taskOutputLoadedMsg{request: d.outputRequest, result: result, lines: lines, maxWidth: maxWidth})
		require.Zero(t, d.terminalScroll)
		require.Zero(t, d.terminalUnseenLines)
		result.Task.State.Status = managedtask.StatusCompleted
		d.HandleMsg(taskOutputLoadedMsg{request: d.outputRequest, result: result, lines: lines, maxWidth: maxWidth})
		draw()
		require.Contains(t, d.panelOutputPosition(width-4), "of 33")
		d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyHome})
		require.Greater(t, d.terminalScroll, 0)
		d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnd})
		require.Zero(t, d.terminalScroll)
		d.ClosePanel()
	}
}

func TestTaskPanelEmptyOutputAndCompactActions(t *testing.T) {
	for _, status := range []managedtask.Status{managedtask.StatusPending, managedtask.StatusRunning, managedtask.StatusCompleted, managedtask.StatusFailed, managedtask.StatusKilled, managedtask.StatusLost} {
		for _, taskType := range []managedtask.Type{managedtask.TypeAgent, managedtask.TypeShell, managedtask.TypeImage} {
			task := managedtask.View{ID: "one", Type: taskType, ChildSessionID: "child", State: managedtask.State{Status: status}}
			theme := styles.ThemeForProvider("")
			d := NewPreviewTasks(&common.Common{Styles: &theme}, []managedtask.View{task}, true)
			screen := uv.NewScreenBuffer(40, 10)
			d.DrawPanel(screen, image.Rect(0, 0, 40, 10), theme.Logo.TitleColorB)
			require.Empty(t, d.panelLines)
			var footer strings.Builder
			for x := 0; x < 40; x++ {
				footer.WriteString(screen.CellAt(x, 9).Content)
				for y := 1; y < 8; y++ {
					background := theme.TaskPanel.OutputBackground
					if x == 0 || x == 39 {
						background = theme.PanelBackground
					}
					require.Equal(t, color.RGBAModel.Convert(background), color.RGBAModel.Convert(screen.CellAt(x, y).Style.Bg))
				}
			}
			require.Contains(t, footer.String(), "esc back")
			if !status.Terminal() {
				require.Contains(t, footer.String(), "s stop")
			} else if taskType == managedtask.TypeAgent {
				require.Contains(t, footer.String(), "c continue")
			}
			d.ClosePanel()
		}
	}
}

type restartPanelWorkspace struct {
	*tasksTestWorkspace
	manager *shell.BackgroundShellManager
}

func (w *restartPanelWorkspace) RestartTask(ctx context.Context, id string) (managedtask.View, error) {
	background, err := w.manager.Restart(ctx, id)
	if err != nil {
		return managedtask.View{}, err
	}
	return managedtask.View{ID: id, Type: managedtask.TypeShell, State: background.State(), OutputRef: background.OutputRef()}, nil
}

func (w *restartPanelWorkspace) TaskOutput(ctx context.Context, id string, wait bool, timeout time.Duration) (managedtask.OutputResult, error) {
	background, ok := w.manager.Get(id)
	if !ok {
		return managedtask.OutputResult{}, fmt.Errorf("task missing: %s", id)
	}
	output, status, err := background.ReadOutput(ctx, managedtask.ReadOptions{Stream: managedtask.OutputStreamMerged}, wait, timeout)
	return managedtask.OutputResult{Task: managedtask.View{ID: id, Type: managedtask.TypeShell, State: background.State(), OutputRef: background.OutputRef()}, Output: string(output.Output), RetrievalStatus: status}, err
}

func TestTaskPanelRestartRealCommand(t *testing.T) {
	manager := shell.NewBackgroundShellManager(t.TempDir())
	defer manager.KillAll(context.Background())
	original, err := manager.Start(t.Context(), t.TempDir(), nil, "if test -f marker; then printf second-run; else printf started > marker; printf first-run; fi; sleep 30", "restart panel")
	require.NoError(t, err)
	require.Eventually(t, func() bool { output, _, _, _ := original.GetOutput(); return output == "first-run" }, 3*time.Second, 10*time.Millisecond)
	theme := styles.ThemeForProvider("")
	ws := &restartPanelWorkspace{tasksTestWorkspace: &tasksTestWorkspace{}, manager: manager}
	d := NewTasksPanel(&common.Common{Workspace: ws, Styles: &theme})
	defer d.ClosePanel()
	d.tasks = []managedtask.View{{ID: original.ID, Type: managedtask.TypeShell, State: original.State(), OutputRef: original.OutputRef()}}
	d.mode = taskDialogDetail
	d.loading = false
	action := d.HandleMsg(tea.KeyPressMsg{Code: 'R', Text: "R"}).(ActionCmd)
	reply := action.Cmd().(taskRestartedMsg)
	require.NoError(t, reply.err)
	require.Equal(t, original.ID, reply.task.ID)
	require.Equal(t, managedtask.StatusKilled, original.Status())
	current, ok := manager.Get(original.ID)
	require.True(t, ok)
	require.Eventually(t, func() bool { output, _, _, _ := current.GetOutput(); return output == "second-run" }, 3*time.Second, 10*time.Millisecond)
	outputAction := d.HandleMsg(reply).(ActionCmd)
	d.HandleMsg(outputAction.Cmd())
	screen := uv.NewScreenBuffer(80, 12)
	d.DrawPanel(screen, image.Rect(0, 0, 80, 12), theme.Logo.TitleColorB)
	var painted strings.Builder
	for y := 0; y < 12; y++ {
		for x := 0; x < 80; x++ {
			painted.WriteString(screen.CellAt(x, y).Content)
		}
	}
	require.Contains(t, painted.String(), "second-run")
	require.NotContains(t, painted.String(), "first-run")
	require.False(t, current.IsDone())
}

type liveOutputPanelWorkspace struct {
	*tasksTestWorkspace
	background          *shell.BackgroundShell
	notificationStarted chan struct{}
	notificationRelease chan struct{}
}

func (w *liveOutputPanelWorkspace) TaskOutput(ctx context.Context, id string, wait bool, timeout time.Duration) (managedtask.OutputResult, error) {
	result, status, err := w.background.ReadOutput(ctx, managedtask.ReadOptions{Stream: managedtask.OutputStreamMerged}, wait, timeout)
	return managedtask.OutputResult{Task: managedtask.View{ID: id, Type: managedtask.TypeShell, State: w.background.State()}, Output: string(result.Output), RetrievalStatus: status, NextOffset: result.NextOffset}, err
}

func (w *liveOutputPanelWorkspace) ListTaskNotifications(ctx context.Context, _ string, _ bool) ([]managedtask.Notification, error) {
	select {
	case w.notificationStarted <- struct{}{}:
	default:
	}
	select {
	case <-w.notificationRelease:
		return nil, fmt.Errorf("notification lookup unavailable")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestTaskPanelRealShellOutputWhileNotificationsBlocked(t *testing.T) {
	manager := shell.NewBackgroundShellManager(t.TempDir())
	defer manager.KillAll(context.Background())
	background, err := manager.Start(t.Context(), t.TempDir(), nil, "printf 'stdout-first\\n'; printf 'stderr-first\\n' >&2; sleep 1; printf 'stdout-later\\n'; printf 'stderr-later\\n' >&2; sleep 10", "live output")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		stdout, stderr, done, err := background.GetOutput()
		return err == nil && !done && strings.Contains(stdout, "stdout-first") && strings.Contains(stderr, "stderr-first")
	}, time.Second, 10*time.Millisecond)
	ws := &liveOutputPanelWorkspace{tasksTestWorkspace: &tasksTestWorkspace{tasks: []managedtask.View{{ID: background.ID, Type: managedtask.TypeShell, State: background.State()}}}, background: background, notificationStarted: make(chan struct{}, 8), notificationRelease: make(chan struct{})}
	theme := styles.ThemeForProvider("")
	d := NewTasksPanel(&common.Common{Workspace: ws, Styles: &theme})
	defer d.ClosePanel()
	d.HandleMsg(d.InitialCmd()())
	results := make(chan tea.Msg, 8)
	launch := func(action Action) {
		cmd := action.(ActionCmd).Cmd
		go func() {
			msg := cmd()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, child := range batch {
					go func() { results <- child() }()
				}
			} else {
				results <- msg
			}
		}()
	}
	launch(d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	var next Action
	select {
	case msg := <-results:
		next = d.HandleMsg(msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("live shell output was blocked by notification lookup")
	}
	require.Contains(t, strings.Join(d.panelLines, "\n"), "stdout-first")
	require.Contains(t, strings.Join(d.panelLines, "\n"), "stderr-first")
	require.NotNil(t, next)
	require.False(t, background.IsDone())
	require.Eventually(t, func() bool {
		stdout, stderr, done, _ := background.GetOutput()
		return !done && strings.Contains(stdout, "stdout-later") && strings.Contains(stderr, "stderr-later")
	}, 2*time.Second, 10*time.Millisecond)
	launch(next)
	select {
	case <-ws.notificationStarted:
	case <-time.After(time.Second):
		t.Fatal("notification lookup did not start")
	}
	deadline := time.After(2 * time.Second)
	for !strings.Contains(strings.Join(d.panelLines, "\n"), "stderr-later") {
		select {
		case msg := <-results:
			if action := d.HandleMsg(msg); action != nil {
				launch(action)
			}
		case <-deadline:
			t.Fatal("live output refresh did not arrive")
		}
	}
	screen := uv.NewScreenBuffer(80, 12)
	d.DrawPanel(screen, image.Rect(0, 0, 80, 12), theme.Logo.TitleColorB)
	var painted strings.Builder
	for y := 1; y < 10; y++ {
		for x := 0; x < 80; x++ {
			painted.WriteString(screen.CellAt(x, y).Content)
		}
	}
	for _, marker := range []string{"stdout-first", "stderr-first", "stdout-later", "stderr-later"} {
		require.Contains(t, painted.String(), marker)
	}
	require.False(t, background.IsDone())
	close(ws.notificationRelease)
}
