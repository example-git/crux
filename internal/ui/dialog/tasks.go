package dialog

import (
	"context"
	"fmt"
	"image"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/pubsub"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/chat"
	"github.com/example-git/crux/internal/ui/common"
)

const (
	TasksID              = "tasks"
	tasksDialogMaxWidth  = 82
	tasksDialogMaxHeight = 28
	taskDetailRefresh    = time.Second
)

type taskDialogMode int

const (
	taskDialogList taskDialogMode = iota
	taskDialogDetail
	taskDialogContinue
)

type tasksLoadedMsg struct {
	tasks []managedtask.View
	err   error
}

type taskOutputLoadedMsg struct {
	request       uint64
	lines         []string
	maxWidth      int
	result        managedtask.OutputResult
	messages      []message.Message
	notifications []managedtask.Notification
	err           error
}

type taskDetailTickMsg struct {
	taskID string
}

type taskStoppedMsg struct {
	request uint64
	task    managedtask.View
	err     error
}

type taskRestartedMsg struct {
	request uint64
	task    managedtask.View
	err     error
}

type taskContinuedMsg struct {
	request uint64
	task    managedtask.View
	err     error
}

type taskNotificationReadMsg struct {
	notification managedtask.Notification
	err          error
}

type Tasks struct {
	now                       func() time.Time
	panel                     bool
	listLoading               bool
	ctx                       context.Context
	cancel                    context.CancelFunc
	outputCancel              context.CancelFunc
	outputRequest             uint64
	actionRequest             uint64
	panelLines                []string
	panelMaxWidth             int
	panelRect                 uv.Rectangle
	panelListStart            int
	panelButtons              []taskPanelButton
	panelNotificationsRequest uint64
	panelNotificationsLoading bool
	panelNotificationsTaskID  string
	panelNotificationsErr     error
	com                       *common.Common
	help                      help.Model
	input                     textinput.Model
	mode                      taskDialogMode
	tasks                     []managedtask.View
	notifications             []managedtask.Notification
	selected                  int
	output                    managedtask.OutputResult
	messages                  []message.Message
	loadErr                   error
	actionErr                 error
	loading                   bool
	refreshing                bool
	terminalFocused           bool
	terminalScroll            int
	terminalUnseenLines       int
	terminalXOffset           int
	terminalContentWidth      int
	terminalViewportHeight    int
	terminalRect              uv.Rectangle
	keyMap                    struct {
		Select        key.Binding
		Next          key.Binding
		Previous      key.Binding
		UpDown        key.Binding
		Refresh       key.Binding
		Stop          key.Binding
		Restart       key.Binding
		Continue      key.Binding
		Back          key.Binding
		Close         key.Binding
		TerminalFocus key.Binding
	}
}

var _ Dialog = (*Tasks)(nil)

func NewTasks(com *common.Common) *Tasks {
	tasks := &Tasks{com: com, loading: true, ctx: context.Background()}
	tasks.help = help.New()
	tasks.help.Styles = com.Styles.DialogHelpStyles()
	tasks.input = textinput.New()
	tasks.input.SetVirtualCursor(false)
	tasks.input.Placeholder = "Continuation prompt"
	tasks.input.CharLimit = 16 << 10
	tasks.input.SetStyles(com.Styles.TextInput)
	tasks.keyMap.Select = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "details"))
	tasks.keyMap.Next = key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("↓", "next"))
	tasks.keyMap.Previous = key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("↑", "previous"))
	tasks.keyMap.UpDown = key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑/↓", "choose"))
	tasks.keyMap.Refresh = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh"))
	tasks.keyMap.Stop = key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "stop"))
	tasks.keyMap.Restart = key.NewBinding(key.WithKeys("R", "shift+r"), key.WithHelp("shift+r", "restart"))
	tasks.keyMap.Continue = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "continue"))
	tasks.keyMap.Back = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back"))
	tasks.keyMap.Close = key.NewBinding(key.WithKeys("esc", "ctrl+c"), key.WithHelp("esc", "close"))
	tasks.keyMap.TerminalFocus = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus output"))
	return tasks
}

func (d *Tasks) ID() string {
	return TasksID
}

func (d *Tasks) InitialCmd() tea.Cmd {
	return d.loadCmd()
}

func (d *Tasks) loadCmd() tea.Cmd {
	if d.panel && d.listLoading {
		return nil
	}
	d.listLoading = true
	return func() tea.Msg {
		tasks, err := d.com.Workspace.ListTasks(d.ctx)
		if err != nil {
			return tasksLoadedMsg{err: err}
		}
		return tasksLoadedMsg{tasks: tasks}
	}
}

func (d *Tasks) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case taskPanelListTickMsg:
		if d.panel && d.mode == taskDialogList && !d.loading {
			return ActionCmd{Cmd: d.loadCmd()}
		}
	case tasksLoadedMsg:
		d.listLoading = false
		if d.panel && d.mode != taskDialogList {
			return nil
		}
		d.loading = false
		d.loadErr = msg.err
		if msg.err == nil {
			selectedID := ""
			if task, ok := d.selectedTask(); ok {
				selectedID = task.ID
			}
			d.tasks = msg.tasks
			d.sortTasks()
			d.selected = 0
			if selectedID != "" {
				d.selectTask(selectedID)
			}
			if !d.panel && len(d.tasks) == 1 && d.mode == taskDialogList {
				d.mode = taskDialogDetail
				return d.loadSelectedOutput(true)
			}
			if d.mode != taskDialogList {
				return d.loadSelectedOutput(false)
			}
		}
	case taskOutputLoadedMsg:
		if d.panel && (msg.request != d.outputRequest || d.mode == taskDialogList || d.ctx.Err() != nil) {
			return nil
		}
		d.loading = false
		d.refreshing = false
		d.actionErr = msg.err
		if msg.err == nil {
			if d.output.Task.OutputRef != msg.result.Task.OutputRef {
				d.terminalScroll = 0
				d.terminalUnseenLines = 0
				d.terminalXOffset = 0
			}
			if d.terminalScroll > 0 && d.output.Task.ID == msg.result.Task.ID {
				oldLines, newLines := len(d.panelLines), len(msg.lines)
				if !d.panel {
					oldLines = len(terminalOutputLines(d.output.Output))
					newLines = len(terminalOutputLines(msg.result.Output))
				}
				added := max(0, newLines-oldLines)
				if msg.result.Task.Type == managedtask.TypeShell && msg.result.NextOffset > int64(len(msg.result.Output)) {
					newBytes := min(max(0, msg.result.NextOffset-d.output.NextOffset), int64(len(msg.result.Output)))
					added = strings.Count(msg.result.Output[int64(len(msg.result.Output))-newBytes:], "\n")
				}
				d.terminalScroll += added
				d.terminalUnseenLines += added
			}
			d.output = msg.result
			d.panelLines = msg.lines
			d.panelMaxWidth = msg.maxWidth
			d.messages = msg.messages
			if !d.panel {
				d.notifications = msg.notifications
			}
			d.replaceTask(msg.result.Task)
			var commands []tea.Cmd
			if d.panel {
				commands = append(commands, d.loadPanelNotifications(msg.result.Task))
			}
			if !msg.result.Task.State.Status.Terminal() {
				commands = append(commands, scheduleTaskDetailRefresh(msg.result.Task.ID))
			}
			return ActionCmd{Cmd: tea.Batch(commands...)}
		}
		if d.panel {
			if task, ok := d.selectedTask(); ok {
				return ActionCmd{Cmd: scheduleTaskDetailRefresh(task.ID)}
			}
		}
	case taskPanelNotificationsMsg:
		if !d.panel || msg.request != d.panelNotificationsRequest || d.mode == taskDialogList || d.ctx.Err() != nil {
			return nil
		}
		d.panelNotificationsLoading = false
		d.panelNotificationsErr = msg.err
		if msg.err == nil {
			d.notifications = msg.notifications
			if msg.terminal {
				d.panelNotificationsTaskID = msg.taskID
			}
		}
		if task, ok := d.selectedTask(); ok && task.ID == msg.taskID && task.State.Status.Terminal() && !msg.terminal {
			return ActionCmd{Cmd: d.loadPanelNotifications(task)}
		}
	case taskDetailTickMsg:
		task, ok := d.selectedTask()
		if d.mode == taskDialogDetail && ok && task.ID == msg.taskID && (!task.State.Status.Terminal() || d.actionErr != nil) && !d.loading && !d.refreshing {
			return d.loadSelectedOutput(false)
		}
	case taskRestartedMsg:
		if msg.request != d.actionRequest || d.mode != taskDialogDetail || d.ctx.Err() != nil {
			return nil
		}
		d.loading = false
		d.actionErr = msg.err
		if msg.err == nil {
			d.replaceTask(msg.task)
			d.output = managedtask.OutputResult{}
			d.panelLines = nil
			d.terminalScroll = 0
			d.terminalUnseenLines = 0
			d.terminalXOffset = 0
			return d.loadSelectedOutput(true)
		}
		if task, ok := d.selectedTask(); ok {
			return ActionCmd{Cmd: scheduleTaskDetailRefresh(task.ID)}
		}
	case taskStoppedMsg:
		if d.panel && msg.request != d.actionRequest {
			return nil
		}
		d.loading = false
		d.actionErr = msg.err
		if msg.err == nil {
			d.replaceTask(msg.task)
			return d.loadSelectedOutput(false)
		}
	case taskContinuedMsg:
		if d.panel && msg.request != d.actionRequest {
			return nil
		}
		d.loading = false
		d.actionErr = msg.err
		if msg.err == nil {
			d.replaceTask(msg.task)
			d.selectTask(msg.task.ID)
			d.mode = taskDialogDetail
			d.terminalFocused = d.panel
			d.input.SetValue("")
			d.input.Blur()
			return d.loadSelectedOutput(true)
		}
	case taskNotificationReadMsg:
		if msg.err != nil {
			d.actionErr = msg.err
			break
		}
		for i := range d.notifications {
			if d.notifications[i].ID == msg.notification.ID {
				d.notifications[i] = msg.notification
			}
		}
	case pubsub.Event[managedtask.Notification]:
		if d.panel && d.mode != taskDialogList {
			if !d.loading && !d.refreshing && d.mode == taskDialogDetail {
				return d.loadSelectedOutput(false)
			}
			return nil
		}
		return ActionCmd{Cmd: d.loadCmd()}
	case common.CoalescedWheelMsg:
		if d.panel && d.mode == taskDialogList && image.Pt(msg.Mouse.X, msg.Mouse.Y).In(d.terminalRect) {
			d.selected = min(max(0, len(d.tasks)-1), max(0, d.selected+int(msg.DeltaY)))
			return nil
		}
		task, ok := d.selectedTask()
		if d.mode == taskDialogDetail && ok && (d.panel || task.Type == managedtask.TypeShell) && image.Pt(msg.Mouse.X, msg.Mouse.Y).In(d.terminalRect) {
			d.scrollTerminal(-int(msg.DeltaY))
			d.scrollTerminalHorizontal(int(msg.DeltaX))
		}
	case tea.MouseClickMsg:
		if d.panel {
			return d.handlePanelClick(msg)
		}
	case tea.MouseWheelMsg:
		return d.handleMouseWheel(msg)
	case tea.KeyPressMsg:
		return d.handleKey(msg)
	default:
		if d.mode == taskDialogContinue {
			var cmd tea.Cmd
			d.input, cmd = d.input.Update(msg)
			return ActionCmd{Cmd: cmd}
		}
	}
	if _, ok := msg.(tasksLoadedMsg); ok && d.panel && d.mode == taskDialogList {
		return ActionCmd{Cmd: scheduleTaskPanelListRefresh()}
	}
	return nil
}

func (d *Tasks) scrollTerminal(delta int) {
	lines := d.outputLines()
	maximum := max(0, len(lines)-max(1, d.terminalViewportHeight))
	d.terminalScroll = min(maximum, max(0, d.terminalScroll+delta))
	d.terminalUnseenLines = min(d.terminalUnseenLines, d.terminalScroll)
}

func (d *Tasks) scrollTerminalHorizontal(delta int) {
	maximumWidth := d.panelMaxWidth
	if !d.panel {
		for _, line := range terminalOutputLines(d.output.Output) {
			maximumWidth = max(maximumWidth, ansi.StringWidth(line))
		}
	}
	maximum := max(0, maximumWidth-max(1, d.terminalContentWidth))
	d.terminalXOffset = min(maximum, max(0, d.terminalXOffset+delta))
}

func (d *Tasks) handleMouseWheel(msg tea.MouseWheelMsg) Action {
	if d.panel && d.mode == taskDialogList && image.Pt(msg.X, msg.Y).In(d.terminalRect) {
		switch msg.Button {
		case tea.MouseWheelUp:
			d.selected = max(0, d.selected-1)
		case tea.MouseWheelDown:
			d.selected = min(max(0, len(d.tasks)-1), d.selected+1)
		}
		return nil
	}
	task, ok := d.selectedTask()
	if d.mode != taskDialogDetail || !ok || (!d.panel && task.Type != managedtask.TypeShell) || !image.Pt(msg.X, msg.Y).In(d.terminalRect) {
		return nil
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		d.scrollTerminal(3)
	case tea.MouseWheelDown:
		d.scrollTerminal(-3)
	}
	return nil
}

func (d *Tasks) handleTerminalKey(msg tea.KeyPressMsg) bool {
	if !d.terminalFocused {
		return false
	}
	switch msg.String() {
	case "up":
		d.scrollTerminal(1)
	case "down":
		d.scrollTerminal(-1)
	case "left", "shift+left":
		d.scrollTerminalHorizontal(-8)
	case "right", "shift+right":
		d.scrollTerminalHorizontal(8)
	case "pgup":
		d.scrollTerminal(max(1, d.terminalViewportHeight-1))
	case "pgdown":
		d.scrollTerminal(-max(1, d.terminalViewportHeight-1))
	case "home":
		d.scrollTerminal(len(d.outputLines()))
	case "end":
		d.terminalScroll = 0
		d.terminalUnseenLines = 0
	default:
		return false
	}
	return true
}

func (d *Tasks) handleKey(msg tea.KeyPressMsg) Action {
	if d.panel && msg.String() == "ctrl+c" {
		return ActionClose{}
	}
	if d.mode == taskDialogContinue {
		switch {
		case key.Matches(msg, d.keyMap.Back):
			d.mode = taskDialogDetail
			d.actionRequest++
			d.loading = false
			d.input.Blur()
			if d.panel {
				if task, ok := d.selectedTask(); ok {
					return ActionCmd{Cmd: scheduleTaskDetailRefresh(task.ID)}
				}
			}
			return nil
		case key.Matches(msg, d.keyMap.Select):
			prompt := strings.TrimSpace(d.input.Value())
			if prompt == "" || d.loading {
				return nil
			}
			task, ok := d.selectedTask()
			if !ok {
				return nil
			}
			d.loading = true
			d.actionErr = nil
			d.actionRequest++
			request := d.actionRequest
			return ActionCmd{Cmd: func() tea.Msg {
				continued, err := d.com.Workspace.ContinueTask(d.ctx, task.ID, task.Ownership.ParentSessionID, prompt)
				return taskContinuedMsg{request: request, task: continued, err: err}
			}}
		default:
			var cmd tea.Cmd
			d.input, cmd = d.input.Update(msg)
			return ActionCmd{Cmd: cmd}
		}
	}

	if d.mode == taskDialogDetail {
		switch {
		case key.Matches(msg, d.keyMap.Back):
			if !d.panel && len(d.tasks) == 1 {
				return ActionClose{}
			}
			d.mode = taskDialogList
			d.actionRequest++
			d.outputRequest++
			if d.outputCancel != nil {
				d.outputCancel()
			}
			d.loading = false
			d.refreshing = false
			d.actionErr = nil
			d.terminalFocused = false
			d.terminalScroll = 0
			d.terminalUnseenLines = 0
			d.terminalXOffset = 0
			if d.panel {
				return ActionCmd{Cmd: scheduleTaskPanelListRefresh()}
			}
			return nil
		case key.Matches(msg, d.keyMap.TerminalFocus):
			task, ok := d.selectedTask()
			if ok && !d.panel && task.Type == managedtask.TypeShell {
				d.terminalFocused = !d.terminalFocused
			}
			return nil
		case d.handleTerminalKey(msg):
			return nil
		case key.Matches(msg, d.keyMap.Refresh):
			if d.refreshing || d.loading {
				return nil
			}
			return d.loadSelectedOutput(false)
		case key.Matches(msg, d.keyMap.Restart):
			task, ok := d.selectedTask()
			if !ok || task.Type != managedtask.TypeShell || d.loading {
				return nil
			}
			d.loading = true
			d.refreshing = false
			d.actionErr = nil
			d.actionRequest++
			d.outputRequest++
			d.panelNotificationsRequest++
			d.panelNotificationsLoading = false
			d.panelNotificationsTaskID = ""
			if d.outputCancel != nil {
				d.outputCancel()
			}
			request := d.actionRequest
			return ActionCmd{Cmd: func() tea.Msg {
				restarted, err := d.com.Workspace.RestartTask(d.ctx, task.ID)
				return taskRestartedMsg{request: request, task: restarted, err: err}
			}}
		case key.Matches(msg, d.keyMap.Stop):
			task, ok := d.selectedTask()
			if !ok || task.State.Status.Terminal() || d.loading {
				return nil
			}
			d.loading = true
			d.actionErr = nil
			d.actionRequest++
			request := d.actionRequest
			return ActionCmd{Cmd: func() tea.Msg {
				stopped, err := d.com.Workspace.StopTask(d.ctx, task.ID)
				return taskStoppedMsg{request: request, task: stopped, err: err}
			}}
		case key.Matches(msg, d.keyMap.Continue):
			task, ok := d.selectedTask()
			if !ok || task.Type != managedtask.TypeAgent || !task.State.Status.Terminal() || task.ChildSessionID == "" || d.loading {
				return nil
			}
			d.mode = taskDialogContinue
			if cmd := d.input.Focus(); cmd != nil {
				return ActionCmd{Cmd: cmd}
			}
			return nil
		}
		return nil
	}

	switch {
	case key.Matches(msg, d.keyMap.Close):
		return ActionClose{}
	case key.Matches(msg, d.keyMap.Previous):
		if len(d.tasks) > 0 {
			d.selected = (d.selected - 1 + len(d.tasks)) % len(d.tasks)
		}
	case key.Matches(msg, d.keyMap.Next):
		if len(d.tasks) > 0 {
			d.selected = (d.selected + 1) % len(d.tasks)
		}
	case key.Matches(msg, d.keyMap.Select):
		if len(d.tasks) > 0 {
			d.mode = taskDialogDetail
			d.terminalFocused = d.panel
			return d.loadSelectedOutput(true)
		}
	case key.Matches(msg, d.keyMap.Refresh):
		d.loading = true
		return ActionCmd{Cmd: d.loadCmd()}
	}
	return nil
}

func (d *Tasks) loadSelectedOutput(initial bool) Action {
	task, ok := d.selectedTask()
	if !ok {
		return nil
	}
	d.outputRequest++
	request := d.outputRequest
	if d.outputCancel != nil {
		d.outputCancel()
	}
	ctx, cancel := context.WithCancel(d.ctx)
	d.outputCancel = cancel
	if initial {
		d.panelLines = nil
		d.panelMaxWidth = 0
		d.panelNotificationsRequest++
		d.panelNotificationsLoading = false
		d.panelNotificationsTaskID = ""
		d.panelNotificationsErr = nil
		d.terminalScroll = 0
		d.terminalUnseenLines = 0
		d.terminalXOffset = 0
		d.loading = true
		d.output = managedtask.OutputResult{}
		d.messages = nil
	} else {
		d.refreshing = true
	}
	d.actionErr = nil
	panel := d.panel
	var commands []tea.Cmd
	commands = append(commands, func() tea.Msg {
		result, err := d.com.Workspace.TaskOutput(ctx, task.ID, false, 0)
		if err != nil {
			return taskOutputLoadedMsg{request: request, result: result, err: err}
		}
		var messages []message.Message
		if result.Task.Type == managedtask.TypeAgent && result.Task.ChildSessionID != "" {
			messages, err = d.com.Workspace.ListMessages(ctx, result.Task.ChildSessionID)
			if err != nil {
				return taskOutputLoadedMsg{request: request, result: result, err: err}
			}
		}
		if panel {
			lines, maxWidth := prepareTaskPanelOutput(result, messages)
			return taskOutputLoadedMsg{request: request, result: result, messages: messages, lines: lines, maxWidth: maxWidth}
		}
		notifications, err := d.com.Workspace.ListTaskNotifications(ctx, result.Task.Ownership.ParentSessionID, true)
		if err != nil {
			return taskOutputLoadedMsg{request: request, result: result, messages: messages, err: err}
		}
		selectedNotifications := make([]managedtask.Notification, 0, 1)
		for _, notification := range notifications {
			if notification.TaskID != task.ID {
				continue
			}
			if notification.ReadAt.IsZero() {
				notification, err = d.com.Workspace.MarkTaskNotificationRead(ctx, notification.ID)
				if err != nil {
					return taskOutputLoadedMsg{request: request, result: result, messages: messages, err: err}
				}
			}
			selectedNotifications = append(selectedNotifications, notification)
		}
		lines, maxWidth := prepareTaskPanelOutput(result, messages)
		return taskOutputLoadedMsg{request: request, result: result, messages: messages, notifications: selectedNotifications, lines: lines, maxWidth: maxWidth}
	})
	return ActionCmd{Cmd: tea.Batch(commands...)}
}

func (d *Tasks) selectedTask() (managedtask.View, bool) {
	if d.selected < 0 || d.selected >= len(d.tasks) {
		return managedtask.View{}, false
	}
	return d.tasks[d.selected], true
}

func (d *Tasks) replaceTask(task managedtask.View) {
	selectedID := ""
	if selected, ok := d.selectedTask(); ok {
		selectedID = selected.ID
	}
	for i := range d.tasks {
		if d.tasks[i].ID == task.ID {
			d.tasks[i] = task
			d.sortTasks()
			d.selectTask(selectedID)
			return
		}
	}
	d.tasks = append(d.tasks, task)
	d.sortTasks()
	d.selectTask(selectedID)
}

func (d *Tasks) selectTask(id string) {
	currentID := ""
	if task, ok := d.selectedTask(); ok {
		currentID = task.ID
	}
	for i := range d.tasks {
		if d.tasks[i].ID == id {
			d.selected = i
			if currentID != id {
				d.terminalFocused = false
				d.terminalScroll = 0
				d.terminalUnseenLines = 0
				d.terminalXOffset = 0
			}
			return
		}
	}
}

func (d *Tasks) sortTasks() {
	sort.SliceStable(d.tasks, func(i, j int) bool {
		return d.tasks[i].State.StartedAt.After(d.tasks[j].State.StartedAt)
	})
}

func (d *Tasks) dialogTitle() string {
	if d.mode == taskDialogList {
		return "Background Tasks"
	}
	task, ok := d.selectedTask()
	if !ok {
		return "Background Tasks"
	}
	if task.Type == managedtask.TypeShell {
		return "Shell details"
	}
	if task.Type == managedtask.TypeImage {
		return "Image details"
	}
	return "Agent details"
}

func (d *Tasks) taskStatusStyle(status managedtask.Status) lipgloss.Style {
	styles := d.com.Styles.Dialog.TaskStatus
	switch status {
	case managedtask.StatusPending:
		return styles.Pending
	case managedtask.StatusRunning:
		return styles.Running
	case managedtask.StatusCompleted:
		return styles.Completed
	case managedtask.StatusFailed:
		return styles.Failed
	case managedtask.StatusKilled:
		return styles.Killed
	case managedtask.StatusLost:
		return styles.Lost
	default:
		return lipgloss.NewStyle()
	}
}

func (d *Tasks) updateTerminalRect(area uv.Rectangle, view string) {
	d.terminalRect = uv.Rectangle{}
	task, ok := d.selectedTask()
	if d.mode != taskDialogDetail || !ok || task.Type != managedtask.TypeShell {
		return
	}
	viewWidth, viewHeight := lipgloss.Size(view)
	center := common.CenterRect(area, min(viewWidth, area.Dx()), min(viewHeight, area.Dy()))
	lines := strings.Split(ansi.Strip(view), "\n")
	for index, line := range lines {
		left := strings.IndexRune(line, '┌')
		right := strings.LastIndex(line, "┐")
		if left < 0 || right <= left {
			continue
		}
		x := center.Min.X + lipgloss.Width(line[:left])
		panelWidth := lipgloss.Width(line[left : right+len("┐")])
		top := center.Min.Y + index
		d.terminalRect = image.Rect(x, top, x+panelWidth, top+d.terminalViewportHeight+2)
		return
	}
}

func (d *Tasks) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(tasksDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(tasksDialogMaxHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	innerWidth := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	renderContext := NewRenderContext(t, width)
	renderContext.Title = d.dialogTitle()
	if task, ok := d.selectedTask(); ok && d.mode == taskDialogDetail && task.Type == managedtask.TypeShell {
		renderContext.ViewStyle = t.Dialog.View.Background(t.Background)
	}
	if d.mode == taskDialogList {
		renderContext.AddPart(d.drawList(innerWidth, height-5))
	} else {
		renderContext.AddPart(d.drawDetail(innerWidth, height-5))
	}
	if d.mode == taskDialogContinue {
		d.input.SetWidth(dialogInputTextWidth(t, d.input, innerWidth))
		renderContext.AddPart(t.Dialog.InputPrompt.Render(d.input.View()))
	}
	if d.loadErr != nil {
		renderContext.AddPart(t.Dialog.TitleError.Render(d.loadErr.Error()))
	} else if d.actionErr != nil {
		renderContext.AddPart(t.Dialog.TitleError.Render(d.actionErr.Error()))
	} else if d.loading {
		renderContext.AddPart(t.Dialog.SecondaryText.Render("Loading…"))
	}
	renderContext.Help = renderDialogHelp(t, &d.help, d, innerWidth)
	view := renderContext.Render()
	var cursor *tea.Cursor
	if d.mode == taskDialogContinue {
		cursor = InputCursor(t, d.input.Cursor())
	}
	d.updateTerminalRect(area, view)
	DrawCenterCursor(scr, area, view, cursor)
	return cursor
}

func (d *Tasks) drawList(width, height int) string {
	t := d.com.Styles
	if len(d.tasks) == 0 {
		return t.Dialog.SecondaryText.Render("No background tasks are currently tracked.")
	}
	visible := max(1, height)
	start := max(0, d.selected-visible/2)
	if start+visible > len(d.tasks) {
		start = max(0, len(d.tasks)-visible)
	}
	end := min(len(d.tasks), start+visible)
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		task := d.tasks[i]
		unread := " "
		for _, notification := range d.notifications {
			if notification.TaskID == task.ID && notification.ReadAt.IsZero() {
				unread = "●"
				break
			}
		}
		description := task.Description
		trailer := "…"
		if task.Type == managedtask.TypeImage {
			description = strings.Join(strings.Fields(description), " ")
			trailer = "..."
		}
		itemStyle := t.Dialog.NormalItem
		if i == d.selected {
			itemStyle = t.Dialog.SelectedItem
		}
		line := fmt.Sprintf("%s %-10s %-9s %s", unread, task.ID, task.State.Status, description)
		lineWidth := max(0, width-t.Dialog.List.GetHorizontalFrameSize()-itemStyle.GetHorizontalFrameSize())
		line = itemStyle.Render(ansi.Truncate(line, lineWidth, trailer))
		lines = append(lines, line)
	}
	return t.Dialog.List.Render(strings.Join(lines, "\n"))
}

func (d *Tasks) drawDetail(width, height int) string {
	task, ok := d.selectedTask()
	if !ok {
		return d.com.Styles.Dialog.SecondaryText.Render("No task selected.")
	}
	if task.Type == managedtask.TypeShell {
		return d.drawShellDetail(width, height, task)
	}
	if task.Type == managedtask.TypeImage {
		return d.drawImageDetail(width, height, task)
	}
	owner := task.Ownership.ParentSessionID
	if task.Ownership.OwnerAgentTaskID != "" {
		owner += " / " + task.Ownership.OwnerAgentTaskID
	}
	lines := []string{
		"ID: " + task.ID,
		"Type: " + string(task.Type),
		"Status: " + string(task.State.Status),
		"Owner: " + owner,
		"Description: " + task.Description,
		"Runtime: " + d.taskRuntime(task).Round(time.Second).String(),
		"Output: " + task.OutputRef,
	}
	if task.AgentType != "" {
		lines = append(lines, "Agent: "+task.AgentType)
	}
	if task.State.ErrorMessage != "" {
		lines = append(lines, "Error: "+task.State.ErrorMessage)
	}
	if task.State.LostReason != "" {
		lines = append(lines, "Lost: "+task.State.LostReason)
	}
	lines = append(lines, fmt.Sprintf("Usage: %d input / %d output tokens · %d tools · $%.4f", task.Usage.PromptTokens, task.Usage.CompletionTokens, task.Usage.ToolUseCount, task.Usage.Cost))
	remaining := max(1, height-len(lines)-1)
	return strings.Join([]string{
		d.com.Styles.Dialog.ContentPanel.Width(width).Render(strings.Join(lines, "\n")),
		d.drawAgentActivity(width, remaining),
	}, "\n")
}

func (d *Tasks) drawImageDetail(width, _ int, task managedtask.View) string {
	status := d.taskStatusStyle(task.State.Status).Render(string(task.State.Status))
	lines := []string{
		"Status: " + status,
		"Runtime: " + d.taskRuntime(task).Round(time.Second).String(),
		"Description: " + task.Description,
	}
	switch task.State.Status {
	case managedtask.StatusPending:
		lines = append(lines, "Waiting in the image queue. Up to four image jobs run at once.")
	case managedtask.StatusRunning:
		lines = append(lines, "Generating image…")
	default:
		if formatted, ok := chat.FormatImagegenResult(d.output.Output); ok {
			lines = append(lines, "", formatted)
		} else if task.State.ErrorMessage != "" {
			lines = append(lines, "Error: "+task.State.ErrorMessage)
		} else if task.State.LostReason != "" {
			lines = append(lines, "Interrupted: "+task.State.LostReason)
		}
	}
	return d.com.Styles.Dialog.ContentPanel.Width(width).Render(strings.Join(lines, "\n"))
}

func (d *Tasks) taskRuntime(task managedtask.View) time.Duration {
	if task.State.StartedAt.IsZero() {
		return 0
	}
	if !task.State.EndedAt.IsZero() {
		return task.State.EndedAt.Sub(task.State.StartedAt)
	}
	if d.now != nil {
		return d.now().Sub(task.State.StartedAt)
	}
	return time.Since(task.State.StartedAt)
}

func (d *Tasks) drawShellDetail(width, height int, task managedtask.View) string {
	command := strings.TrimSpace(task.Command)
	if command == "" {
		command = task.Description
	}
	status := d.taskStatusStyle(task.State.Status).Render(string(task.State.Status))
	metadata := []string{
		"Status: " + status,
		"Runtime: " + d.taskRuntime(task).Round(time.Second).String(),
	}
	commandPanel := d.com.Styles.Dialog.CommandPanel.Width(width).Render("Command: " + command)
	outcome := make([]string, 0, 2)
	if task.State.ExitCode != nil {
		outcome = append(outcome, fmt.Sprintf("Exit code: %d", *task.State.ExitCode))
	}
	if task.State.ErrorMessage != "" {
		outcome = append(outcome, "Error: "+task.State.ErrorMessage)
	}
	if task.State.LostReason != "" {
		outcome = append(outcome, "Lost: "+task.State.LostReason)
	}
	usedHeight := len(metadata) + lipgloss.Height(commandPanel) + len(outcome) + 2
	remaining := max(5, height-usedHeight)
	parts := []string{strings.Join(metadata, "\n"), commandPanel}
	if len(outcome) > 0 {
		parts = append(parts, strings.Join(outcome, "\n"))
	}
	parts = append(parts, "", d.drawShellActivity(width, remaining, task))
	return strings.Join(parts, "\n")
}

func scheduleTaskDetailRefresh(taskID string) tea.Cmd {
	return tea.Tick(taskDetailRefresh, func(time.Time) tea.Msg {
		return taskDetailTickMsg{taskID: taskID}
	})
}

func (d *Tasks) drawShellActivity(width, height int, task managedtask.View) string {
	lines := terminalOutputLines(d.output.Output)
	waiting := len(lines) == 0
	if waiting {
		lines = []string{"Waiting for output…"}
	}
	viewportHeight := max(1, height-4)
	d.terminalViewportHeight = viewportHeight
	maximumScroll := max(0, len(lines)-viewportHeight)
	d.terminalScroll = min(d.terminalScroll, maximumScroll)
	end := max(0, len(lines)-d.terminalScroll)
	start := max(0, end-viewportHeight)
	visible := lines[start:end]
	panelStyle := d.com.Styles.Dialog.TerminalPanel
	if d.terminalFocused {
		panelStyle = d.com.Styles.Dialog.TerminalPanelFocused
	}
	contentWidth := max(1, width-2-panelStyle.GetHorizontalFrameSize())
	d.terminalContentWidth = contentWidth
	d.scrollTerminalHorizontal(0)
	for index := range visible {
		visible[index] = ansi.Cut(visible[index], d.terminalXOffset, d.terminalXOffset+contentWidth)
	}
	panel := panelStyle.Width(max(1, width-2)).Height(viewportHeight).Render(strings.Join(visible, "\n"))
	lineCount := len(lines)
	if waiting {
		lineCount = 0
	}
	shown := min(lineCount, viewportHeight)
	count := fmt.Sprintf("Showing %d %s", shown, pluralizeLines(shown))
	if lineCount > viewportHeight {
		first := start + 1
		last := end
		count = fmt.Sprintf("Showing lines %d-%d of %d", first, last, lineCount)
	}
	if d.terminalFocused {
		count += " · ↑/↓ scroll · ←/→ pan · pgup/pgdown page · end follow"
	}
	return strings.Join([]string{
		d.com.Styles.Dialog.PrimaryText.Padding(0).Render("Output:"),
		panel,
		d.com.Styles.Dialog.SecondaryText.Padding(0).Render(ansi.Truncate(count, max(1, width), "…")),
	}, "\n")
}

func (d *Tasks) drawAgentActivity(width, height int) string {
	if len(d.messages) == 0 {
		return d.com.Styles.Dialog.ContentPanel.Width(width).Render("Agent activity\n\nWaiting for agent activity…")
	}
	messagePointers := make([]*message.Message, len(d.messages))
	for i := range d.messages {
		messagePointers[i] = &d.messages[i]
	}
	toolResults := chat.BuildToolResultMap(messagePointers)
	contentWidth := max(1, width-4)
	var rendered []string
	for _, msg := range messagePointers {
		for _, item := range chat.ExtractMessageItems(d.com.Styles, msg, toolResults, d.com.Workspace.WorkingDir()) {
			rendered = append(rendered, item.Render(contentWidth))
		}
	}
	lines := strings.Split(strings.Join(rendered, "\n"), "\n")
	lines = tailLines(lines, max(1, height-2))
	return d.com.Styles.Dialog.ContentPanel.Width(width).Render("Agent activity\n\n" + strings.Join(lines, "\n"))
}

func pluralizeLines(count int) string {
	if count == 1 {
		return "line"
	}
	return "lines"
}

func terminalOutputLines(output string) []string {
	plain := ansi.Strip(output)
	lineRunes := [][]rune{{}}
	for _, character := range plain {
		last := len(lineRunes) - 1
		switch character {
		case '\r':
			lineRunes[last] = lineRunes[last][:0]
		case '\n':
			lineRunes = append(lineRunes, nil)
		case '\b':
			if len(lineRunes[last]) > 0 {
				lineRunes[last] = lineRunes[last][:len(lineRunes[last])-1]
			}
		default:
			if character >= ' ' || character == '\t' {
				lineRunes[last] = append(lineRunes[last], character)
			}
		}
	}
	if len(lineRunes) > 1 && len(lineRunes[len(lineRunes)-1]) == 0 {
		lineRunes = lineRunes[:len(lineRunes)-1]
	}
	lines := make([]string, len(lineRunes))
	for i := range lineRunes {
		lines[i] = strings.ReplaceAll(string(lineRunes[i]), "\t", "    ")
	}
	return lines
}

func tailLines(lines []string, count int) []string {
	if len(lines) <= count {
		return lines
	}
	return lines[len(lines)-count:]
}

func (d *Tasks) Cursor() *tea.Cursor {
	if d.mode != taskDialogContinue {
		return nil
	}
	return InputCursor(d.com.Styles, d.input.Cursor())
}

func (d *Tasks) ShortHelp() []key.Binding {
	if d.panel {
		if d.mode == taskDialogList {
			return []key.Binding{d.keyMap.Close, d.keyMap.Select, d.keyMap.UpDown, d.keyMap.Refresh}
		}
		if d.mode == taskDialogContinue {
			return []key.Binding{d.keyMap.Back, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "submit"))}
		}
		task, _ := d.selectedTask()
		bindings := []key.Binding{d.keyMap.Back}
		if task.Type == managedtask.TypeShell {
			bindings = append(bindings, d.keyMap.Restart)
		}
		if !task.State.Status.Terminal() {
			bindings = append(bindings, d.keyMap.Stop)
		} else if task.Type == managedtask.TypeAgent && task.ChildSessionID != "" {
			bindings = append(bindings, d.keyMap.Continue)
		}
		return append(bindings, d.keyMap.Refresh, key.NewBinding(key.WithKeys("up", "down", "left", "right"), key.WithHelp("arrows", "scroll")))
	}
	if d.mode == taskDialogList {
		return []key.Binding{d.keyMap.UpDown, d.keyMap.Select, d.keyMap.Refresh, d.keyMap.Close}
	}
	if d.mode == taskDialogContinue {
		return []key.Binding{d.keyMap.Select, d.keyMap.Back}
	}
	task, _ := d.selectedTask()
	bindings := []key.Binding{d.keyMap.Refresh}
	if task.Type == managedtask.TypeShell {
		bindings = append(bindings, d.keyMap.Restart)
	}
	if !d.panel && task.Type == managedtask.TypeShell {
		bindings = append(bindings, d.keyMap.TerminalFocus)
	}
	if !task.State.Status.Terminal() {
		bindings = append(bindings, d.keyMap.Stop)
	}
	if task.Type == managedtask.TypeAgent && task.State.Status.Terminal() && task.ChildSessionID != "" {
		bindings = append(bindings, d.keyMap.Continue)
	}
	return append(bindings, d.keyMap.Back)
}

func (d *Tasks) FullHelp() [][]key.Binding {
	return [][]key.Binding{d.ShortHelp()}
}
