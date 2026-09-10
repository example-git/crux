package dialog

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/message"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/chat"
	"github.com/example-git/crux/internal/ui/common"
)

type taskPanelListTickMsg struct{}

type taskPanelNotificationsMsg struct {
	request       uint64
	taskID        string
	terminal      bool
	notifications []managedtask.Notification
	err           error
}

func (d *Tasks) loadPanelNotifications(task managedtask.View) tea.Cmd {
	if d.panelNotificationsLoading || d.panelNotificationsTaskID == task.ID {
		return nil
	}
	d.panelNotificationsLoading = true
	request := d.panelNotificationsRequest
	ctx := d.ctx
	ws := d.com.Workspace
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		msg := taskPanelNotificationsMsg{request: request, taskID: task.ID, terminal: task.State.Status.Terminal()}
		notifications, err := ws.ListTaskNotifications(ctx, task.Ownership.ParentSessionID, true)
		if err != nil {
			msg.err = err
			return msg
		}
		for _, notification := range notifications {
			if notification.TaskID != task.ID {
				continue
			}
			if notification.ReadAt.IsZero() {
				notification, err = ws.MarkTaskNotificationRead(ctx, notification.ID)
				if err != nil {
					msg.err = err
					return msg
				}
			}
			msg.notifications = append(msg.notifications, notification)
		}
		return msg
	}
}

type taskPanelButton struct {
	rect image.Rectangle
	key  tea.KeyPressMsg
}

func NewTasksPanel(com *common.Common) *Tasks {
	d := NewTasks(com)
	d.panel = true
	d.ctx, d.cancel = context.WithCancel(context.Background())
	return d
}

func (d *Tasks) PanelInfoLines() []string {
	if d.mode == taskDialogList {
		return nil
	}
	task, ok := d.selectedTask()
	if !ok {
		return nil
	}
	title := strings.Join(strings.Fields(task.Description), " ")
	if title == "" {
		title = string(task.Type) + " task"
	}
	lines := []string{
		title,
		fmt.Sprintf("%s · %s · %s", task.Type, task.ID, d.taskRuntime(task).Round(time.Second)),
	}
	if task.Command != "" {
		lines = append(lines, "$ "+task.Command)
	} else if task.AgentType != "" {
		lines = append(lines, fmt.Sprintf("%s · %d input / %d output tokens · %d tools", task.AgentType, task.Usage.PromptTokens, task.Usage.CompletionTokens, task.Usage.ToolUseCount))
	}
	return lines
}

func (d *Tasks) RenderPanelInfo(width int, accent color.Color) string {
	lines := d.PanelInfoLines()
	if len(lines) == 0 || width < 2 {
		return ""
	}
	width -= 2
	sty := d.com.Styles
	task, _ := d.selectedTask()
	innerWidth := max(0, width-2)
	status := string(task.State.Status)
	icon := "●"
	switch task.State.Status {
	case managedtask.StatusCompleted:
		icon = "✓"
	case managedtask.StatusFailed, managedtask.StatusKilled, managedtask.StatusLost:
		icon = "×"
	case managedtask.StatusPending:
		icon = "○"
	}
	status = ansi.Truncate(icon+" "+status, innerWidth, "…")
	statusWidth := ansi.StringWidth(status)
	titleWidth := max(0, innerWidth-statusWidth-2)
	title := sty.TaskPanel.Title.Render(ansi.Truncate(lines[0], titleWidth, "…"))
	statusStyle := d.taskStatusStyle(task.State.Status).Padding(0).Background(sty.PanelBackground)
	base := lipgloss.NewStyle().Background(sty.PanelBackground)
	lines[0] = base.Render(" ") + title + base.Render(strings.Repeat(" ", max(0, innerWidth-ansi.StringWidth(title)-statusWidth))) + statusStyle.Render(status) + base.Render(" ")
	for i := 1; i < len(lines); i++ {
		text := ansi.Truncate(strings.Join(strings.Fields(lines[i]), " "), innerWidth, "…")
		if i == 2 && task.Command != "" {
			prompt := lipgloss.NewStyle().Foreground(accent).Background(sty.PanelBackground).Render(ansi.Cut(text, 0, 2))
			command := sty.TaskPanel.Title.Bold(false).Render(ansi.Cut(text, 2, innerWidth))
			lines[i] = base.Render(" ") + prompt + command + base.Render(strings.Repeat(" ", max(0, width-1-ansi.StringWidth(text))))
		} else {
			lines[i] = sty.TaskPanel.Metadata.Width(width).Render(" " + text)
		}
	}
	frame := lipgloss.NewStyle().Foreground(accent).Background(sty.PanelBackground)
	for i, line := range lines {
		lines[i] = frame.Render("│") + base.Render(ansi.Truncate(line, width, "")) + frame.Render("│")
	}
	top := frame.Render("╭" + strings.Repeat("─", width) + "╮")
	return strings.Join(append([]string{top}, lines...), "\n")
}

func (d *Tasks) panelOutputPosition(width int) string {
	total := len(d.panelLines)
	end := max(0, total-d.terminalScroll)
	start := max(0, end-d.terminalViewportHeight)
	first := 0
	if end > 0 {
		first = start + 1
	}
	position := fmt.Sprintf("Lines %d–%d of %d", first, end, total)
	task, _ := d.selectedTask()
	if d.terminalScroll > 0 {
		if d.terminalUnseenLines > 0 {
			position = fmt.Sprintf("Earlier output · %d new lines ↓ · End latest", d.terminalUnseenLines)
			if ansi.StringWidth(position) > width {
				position = fmt.Sprintf("%d new lines ↓ · End latest", d.terminalUnseenLines)
			}
		} else {
			position += " · End latest ↓"
		}
	} else if !task.State.Status.Terminal() {
		position = "Following output ↓"
	}
	if d.output.OutputTruncated {
		position = "Output truncated · " + position
	}
	return ansi.Truncate(position, max(0, width), "…")
}

func (d *Tasks) ClosePanel() {
	if d.cancel != nil {
		d.cancel()
	}
	if d.outputCancel != nil {
		d.outputCancel()
	}
}

func scheduleTaskPanelListRefresh() tea.Cmd {
	return tea.Tick(taskDetailRefresh, func(time.Time) tea.Msg { return taskPanelListTickMsg{} })
}

func (d *Tasks) outputLines() []string {
	if d.panel {
		return d.panelLines
	}
	return terminalOutputLines(d.output.Output)
}

func prepareTaskPanelOutput(result managedtask.OutputResult, messages []message.Message) ([]string, int) {
	task := result.Task
	var text strings.Builder
	if len(messages) > 0 && task.Type == managedtask.TypeAgent {
		for _, msg := range messages {
			if content := msg.ReasoningContent().Thinking; content != "" {
				fmt.Fprintf(&text, "thinking> %s\n", content)
			}
			if content := msg.Content().Text; content != "" {
				fmt.Fprintf(&text, "%s> %s\n", msg.Role, content)
			}
			for _, call := range msg.ToolCalls() {
				fmt.Fprintf(&text, "$ %s %s\n", call.Name, call.Input)
			}
			for _, output := range msg.ToolResults() {
				fmt.Fprintln(&text, output.Content)
			}
		}
	} else if task.Type == managedtask.TypeImage {
		if formatted, ok := chat.FormatImagegenResult(result.Output); ok {
			text.WriteString(formatted)
		} else {
			text.WriteString(result.Output)
		}
	} else {
		text.WriteString(result.Output)
	}
	if text.Len() > 0 && !strings.HasSuffix(text.String(), "\n") {
		text.WriteByte('\n')
	}
	if task.State.ErrorMessage != "" {
		fmt.Fprintln(&text, "Error: "+task.State.ErrorMessage)
	}
	if task.State.LostReason != "" {
		fmt.Fprintln(&text, "Lost: "+task.State.LostReason)
	}
	if task.State.ExitCode != nil {
		fmt.Fprintf(&text, "Exit code: %d\n", *task.State.ExitCode)
	}
	if strings.TrimSpace(text.String()) == "" {
		return nil, 0
	}
	lines := terminalOutputLines(text.String())
	maxWidth := 0
	for _, line := range lines {
		maxWidth = max(maxWidth, ansi.StringWidth(line))
	}
	return lines, maxWidth
}

func (d *Tasks) panelTaskRow(task managedtask.View, width int, selected bool, accent color.Color) string {
	sty := d.com.Styles
	background := sty.Background
	marker := "  "
	if selected {
		background = sty.TaskPanel.ControlsBackground
		marker = "› "
	}
	base := lipgloss.NewStyle().Background(background)
	title := strings.Join(strings.Fields(task.Description), " ")
	if title == "" {
		title = task.ID
	}
	metadata := ""
	if width >= 55 {
		metadata = fmt.Sprintf("  %-6s  %9s  ", task.Type, ansi.Truncate(d.taskRuntime(task).Round(time.Second).String(), 9, "…"))
	}
	if width >= 100 {
		metadata = "  " + task.ID + metadata
	}
	status := fmt.Sprintf("%9s", task.State.Status)
	if width < 24 {
		status = ""
	}
	titleWidth := max(0, width-ansi.StringWidth(marker+metadata+status)-3)
	title = ansi.Truncate(title, titleWidth, "…")
	gap := strings.Repeat(" ", max(0, width-ansi.StringWidth(marker+title+metadata+status)-1))
	line := base.Foreground(accent).Render(marker) + sty.TaskPanel.Title.Bold(selected).Background(background).Render(title) + base.Render(gap)
	line += sty.TaskPanel.Metadata.Background(background).Render(metadata) + d.taskStatusStyle(task.State.Status).Padding(0).Background(background).Render(status) + base.Render(" ")
	return ansi.Truncate(line, max(0, width), "")
}

func (d *Tasks) DrawPanel(scr uv.Screen, area uv.Rectangle, accent color.Color) *tea.Cursor {
	d.panelRect = area
	d.panelButtons = nil
	d.terminalRect = uv.Rectangle{}
	if area.Dx() < 1 || area.Dy() < 1 {
		return nil
	}
	sty := d.com.Styles
	background := sty.PanelBackground
	foreground := sty.Dialog.PrimaryText.GetForeground()
	frameBackground := sty.PanelBackground
	if d.mode == taskDialogList {
		background = sty.Background
		frameBackground = background
		foreground = sty.TaskPanel.OutputForeground
	} else if d.mode != taskDialogContinue {
		background = sty.TaskPanel.OutputBackground
		foreground = sty.TaskPanel.OutputForeground
	}
	base := lipgloss.NewStyle().Foreground(foreground).Background(background)
	uv.NewStyledString(base.Width(area.Dx()).Height(area.Dy()).Render("")).Draw(scr, area)
	if d.mode == taskDialogList && area.Dx() >= 2 && area.Dy() >= 5 {
		frame := lipgloss.NewStyle().Foreground(accent).Background(frameBackground)
		top := "╭" + strings.Repeat("─", area.Dx()-2) + "╮"
		uv.NewStyledString(frame.Render(top)).Draw(scr, image.Rect(area.Min.X, area.Min.Y, area.Max.X, area.Min.Y+1))
		title := ansi.Truncate(fmt.Sprintf(" Background Tasks · %d ", len(d.tasks)), area.Dx()-2, "…")
		line := frame.Render("│") + sty.TaskPanel.Title.Background(frameBackground).Width(area.Dx()-2).Render(title) + frame.Render("│")
		uv.NewStyledString(line).Draw(scr, image.Rect(area.Min.X, area.Min.Y+1, area.Max.X, area.Min.Y+2))
		area.Min.Y += 2
	}
	row := func(y int, text string, style lipgloss.Style) {
		if y < area.Min.Y || y >= area.Max.Y {
			return
		}
		left, right := area.Min.X, area.Max.X
		if y > area.Min.Y {
			left++
			right = max(left, right-1)
		}
		text = ansi.Truncate(strings.Join(strings.Split(text, "\n"), " "), max(0, right-left-2), "…")
		uv.NewStyledString(style.Background(background).Width(max(0, right-left)).Render(" "+text)).Draw(scr, image.Rect(left, y, right, y+1))
	}
	body := image.Rect(area.Min.X+1, area.Min.Y+1, max(area.Min.X+1, area.Max.X-1), max(area.Min.Y+1, area.Max.Y-1))
	if area.Dx() >= 2 && area.Dy() >= 3 {
		frame := lipgloss.NewStyle().Foreground(accent).Background(frameBackground)
		top := "╭" + strings.Repeat("▄", area.Dx()-2) + "╮"
		bottom := "╰" + strings.Repeat("▀", area.Dx()-2) + "╯"
		uv.NewStyledString(frame.Render(top)).Draw(scr, image.Rect(area.Min.X, area.Min.Y, area.Max.X, area.Min.Y+1))
		body.Max.Y--
		uv.NewStyledString(frame.Render(bottom)).Draw(scr, image.Rect(area.Min.X, body.Max.Y, area.Max.X, body.Max.Y+1))
		for y := body.Min.Y; y < body.Max.Y; y++ {
			uv.NewStyledString(frame.Render("▐")).Draw(scr, image.Rect(area.Min.X, y, area.Min.X+1, y+1))
			uv.NewStyledString(frame.Render("▌")).Draw(scr, image.Rect(area.Max.X-1, y, area.Max.X, y+1))
		}
	} else if d.mode == taskDialogList {
		row(area.Min.Y, "Background Tasks", sty.TaskPanel.Title)
	}
	body.Min.X = min(body.Min.X+1, body.Max.X)
	body.Max.X = max(body.Min.X, body.Max.X-1)
	body.Min.Y = min(body.Min.Y+1, body.Max.Y)
	body.Max.Y = max(body.Min.Y, body.Max.Y-1)
	if body.Dy() > 0 && (d.loadErr != nil || d.actionErr != nil || d.loading || d.panelNotificationsErr != nil) {
		status := "Loading…"
		if d.loadErr != nil {
			status = d.loadErr.Error()
		} else if d.actionErr != nil {
			status = d.actionErr.Error()
		} else if !d.loading && d.panelNotificationsErr != nil {
			status = "Notifications: " + d.panelNotificationsErr.Error()
		}
		row(body.Min.Y, status, sty.Dialog.SecondaryText)
		body.Min.Y = min(body.Max.Y, body.Min.Y+1)
	}
	var cursor *tea.Cursor
	if d.mode == taskDialogList {
		visible := max(0, body.Dy())
		d.panelListStart = min(max(0, d.selected-visible/2), max(0, len(d.tasks)-visible))
		if body.Dy() > 0 && len(d.tasks) == 0 && !d.loading {
			row(body.Min.Y, "No background tasks are currently tracked.", sty.Dialog.SecondaryText)
		}
		for i := d.panelListStart; i < min(len(d.tasks), d.panelListStart+visible); i++ {
			task := d.tasks[i]
			line := d.panelTaskRow(task, body.Dx(), i == d.selected, accent)
			y := body.Min.Y + i - d.panelListStart
			uv.NewStyledString(line).Draw(scr, image.Rect(body.Min.X, y, body.Max.X, y+1))
		}
		d.terminalRect = body
	} else if d.mode == taskDialogContinue {
		d.input.SetWidth(max(1, body.Dx()-2))
		uv.NewStyledString(base.Render(d.input.View())).Draw(scr, body)
		cursor = d.input.Cursor()
		if cursor != nil {
			cursor.X += body.Min.X
			cursor.Y += body.Min.Y
		}
	} else {
		d.terminalRect = body
		d.terminalContentWidth = max(1, body.Dx())
		d.terminalViewportHeight = max(1, body.Dy())
		d.scrollTerminal(0)
		d.scrollTerminalHorizontal(0)
		end := max(0, len(d.panelLines)-d.terminalScroll)
		start := max(0, end-d.terminalViewportHeight)
		for i := start; i < end && body.Min.Y+i-start < body.Max.Y; i++ {
			line := ansi.Cut(d.panelLines[i], d.terminalXOffset, d.terminalXOffset+d.terminalContentWidth)
			uv.NewStyledString(base.Width(body.Dx()).Render(line)).Draw(scr, image.Rect(body.Min.X, body.Min.Y+i-start, body.Max.X, body.Min.Y+i-start+1))
		}
		if body.Dy() > 0 && len(d.panelLines) == 0 && !d.loading {
			row(body.Min.Y, "Waiting for output…", sty.Dialog.SecondaryText)
		}
		if area.Dy() >= 3 && area.Dx() > 4 {
			position := " " + d.panelOutputPosition(area.Dx()-4) + " "
			x := area.Max.X - ansi.StringWidth(position) - 1
			rect := image.Rect(x, area.Max.Y-2, area.Max.X-1, area.Max.Y-1)
			uv.NewStyledString(base.Background(sty.PanelBackground).Render(position)).Draw(scr, rect)
			if d.terminalScroll > 0 {
				d.panelButtons = append(d.panelButtons, taskPanelButton{rect: rect, key: tea.KeyPressMsg{Code: tea.KeyEnd}})
			}
		}
	}
	controlsBackground := sty.TaskPanel.ControlsBackground
	if d.mode == taskDialogList {
		controlsBackground = background
	}
	controls := lipgloss.NewStyle().Background(controlsBackground)
	uv.NewStyledString(controls.Width(area.Dx()).Render("")).Draw(scr, image.Rect(area.Min.X, area.Max.Y-1, area.Max.X, area.Max.Y))
	keyStyle := sty.TaskPanel.Title.Bold(false).Background(controlsBackground)
	labelStyle := sty.TaskPanel.Metadata.Background(controlsBackground)
	x := area.Min.X + 1
	for _, binding := range d.ShortHelp() {
		keyLabel := binding.Help().Key
		if keyLabel == "arrows" {
			keyLabel = "↑↓←→"
		}
		label := keyLabel + " " + binding.Help().Desc
		width := ansi.StringWidth(label)
		if x+width >= area.Max.X {
			continue
		}
		if binding.Help().Key == "arrows" {
			x = area.Max.X - width - 1
		}
		button := image.Rect(x, area.Max.Y-1, x+width, area.Max.Y)
		text := keyStyle.Render(keyLabel) + labelStyle.Render(" "+binding.Help().Desc)
		uv.NewStyledString(text).Draw(scr, button)
		press := tea.KeyPressMsg{}
		switch binding.Keys()[0] {
		case "enter":
			press.Code = tea.KeyEnter
		case "esc":
			press.Code = tea.KeyEscape
		case "up":
			press.Code = tea.KeyUp
		default:
			press.Code = rune(binding.Keys()[0][0])
			press.Text = binding.Keys()[0]
		}
		d.panelButtons = append(d.panelButtons, taskPanelButton{rect: button, key: press})
		x += width + 2
	}
	return cursor
}

func (d *Tasks) handlePanelClick(msg tea.MouseClickMsg) Action {
	if msg.Button != tea.MouseLeft {
		return nil
	}
	point := image.Pt(msg.X, msg.Y)
	for _, button := range d.panelButtons {
		if point.In(button.rect) {
			return d.handleKey(button.key)
		}
	}
	if d.mode == taskDialogList && point.In(d.terminalRect) {
		index := d.panelListStart + msg.Y - d.terminalRect.Min.Y
		if index < len(d.tasks) {
			d.selected = index
			return d.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
		}
	}
	return nil
}
