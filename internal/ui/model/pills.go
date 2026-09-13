package model

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/chat"
	"github.com/example-git/crux/internal/ui/styles"
)

const (
	// pillHeightWithBorder is the height of a pill including its border.
	pillHeightWithBorder = 3
	// maxTaskDisplayLength is the maximum length of a task name in the pill.
	maxTaskDisplayLength = 40
	// maxQueueDisplayLength is the maximum length of a queue item in the list.
	maxQueueDisplayLength = 60
)

// pillSection represents which section of the pills panel is focused.
type pillSection int

const (
	pillSectionTodos pillSection = iota
	pillSectionQueue
)

// hasIncompleteTodos returns true if there are any non-completed todos.
func hasIncompleteTodos(todos []session.Todo) bool {
	return session.HasIncompleteTodos(todos)
}

// hasInProgressTodo returns true if there is at least one in-progress todo.
func hasInProgressTodo(todos []session.Todo) bool {
	for _, todo := range todos {
		if todo.Status == session.TodoStatusInProgress {
			return true
		}
	}
	return false
}

// queuePill renders the queue count pill with gradient triangles. Pills always
// render with a border; focus within the expanded panel is conveyed by the list
// shown below the pills, not by hiding a pill's border.
func queuePill(queue int, t *styles.Styles, items ...agent.QueuedPrompt) string {
	if queue <= 0 {
		return ""
	}
	triangles := styles.ForegroundGrad(t.Pills.QueueIconBase, "▶▶▶▶▶▶▶▶▶", false, t.Pills.QueueGradFromColor, t.Pills.QueueGradToColor)
	if queue < len(triangles) {
		triangles = triangles[:queue]
	}

	steering := 0
	for _, item := range items {
		if item.DeliveryMode == agent.DeliverySteer {
			steering++
		}
	}
	var labels []string
	if queued := queue - steering; queued > 0 {
		labels = append(labels, fmt.Sprintf("%d Queued", queued))
	}
	if steering > 0 {
		labels = append(labels, fmt.Sprintf("%d Steering", steering))
	}
	text := t.Pills.QueueLabel.Render(strings.Join(labels, " · "))
	content := fmt.Sprintf("%s %s", strings.Join(triangles, ""), text)
	return t.Pills.Focused.Render(content)
}

// todoPill renders the todo progress pill with optional spinner and task name.
func todoPill(todos []session.Todo, spinnerView string, panelFocused bool, t *styles.Styles) string {
	if !hasIncompleteTodos(todos) {
		return ""
	}

	completed := 0
	var currentTodo *session.Todo
	for i := range todos {
		switch todos[i].Status {
		case session.TodoStatusCompleted:
			completed++
		case session.TodoStatusInProgress:
			if currentTodo == nil {
				currentTodo = &todos[i]
			}
		}
	}

	total := len(todos)

	label := t.Pills.TodoLabel.Render("To-Do")
	progress := t.Pills.TodoProgress.Render(fmt.Sprintf("%d/%d", completed, total))

	var content string
	if panelFocused {
		content = fmt.Sprintf("%s %s", label, progress)
	} else if currentTodo != nil {
		taskText := currentTodo.Content
		if currentTodo.ActiveForm != "" {
			taskText = currentTodo.ActiveForm
		}
		if ansi.StringWidth(taskText) > maxTaskDisplayLength {
			taskText = ansi.Truncate(taskText, maxTaskDisplayLength-1, "…")
		}
		task := t.Pills.TodoCurrentTask.Render(taskText)
		content = fmt.Sprintf("%s %s %s  %s", spinnerView, label, progress, task)
	} else {
		content = fmt.Sprintf("%s %s", label, progress)
	}

	return t.Pills.Focused.Render(content)
}

func visibleTodos(todos []session.Todo) []session.Todo {
	ordered := make([]session.Todo, 0, len(todos))
	for _, status := range []session.TodoStatus{session.TodoStatusCompleted, session.TodoStatusInProgress, session.TodoStatusPending} {
		for _, todo := range todos {
			if todo.Status == status {
				ordered = append(ordered, todo)
			}
		}
	}
	anchor := len(ordered)
	for i, todo := range ordered {
		if todo.Status != session.TodoStatusCompleted {
			anchor = i
			break
		}
	}
	start := max(0, min(anchor-1, len(ordered)-4))
	return ordered[start:min(start+4, len(ordered))]
}

// todoList renders the expanded todo list.
func todoList(sessionTodos []session.Todo, spinnerView string, t *styles.Styles, width int) string {
	todos := append([]session.Todo(nil), sessionTodos...)
	todos = visibleTodos(todos)
	textWidth := max(1, min(80, width-2))
	for i := range todos {
		todos[i].Content = ansi.Truncate(strings.Join(strings.Fields(todos[i].Content), " "), textWidth, "…")
		todos[i].ActiveForm = ansi.Truncate(strings.Join(strings.Fields(todos[i].ActiveForm), " "), textWidth, "…")
	}
	return chat.FormatTodosList(t, todos, spinnerView, width, true)
}

// queueList renders the expanded queue items list.
func queueList(queueItems []agent.QueuedPrompt, t *styles.Styles) string {
	if len(queueItems) == 0 {
		return ""
	}

	var lines []string
	for _, item := range queueItems {
		text := item.Prompt
		if item.DeliveryMode == agent.DeliverySteer {
			text = "Steer: " + text
		}
		if ansi.StringWidth(text) > maxQueueDisplayLength {
			text = ansi.Truncate(text, maxQueueDisplayLength-1, "…")
		}
		prefix := t.Pills.QueueItemPrefix.Render() + " "
		lines = append(lines, prefix+t.Pills.QueueItemText.Render(text))
	}

	return strings.Join(lines, "\n")
}

// pillsHeightReasonableTerminalHeight is the minimum terminal height at which
// we auto-expand pills when there are incomplete todos.
const pillsHeightReasonableTerminalHeight = 40

// autoExpandPillsIfReasonable expands the pills panel if the terminal has
// enough vertical space to show the expanded list comfortably.
func (m *UI) autoExpandPillsIfReasonable() tea.Cmd {
	if !m.hasSession() {
		return nil
	}
	if m.activeInline != nil {
		return nil
	}
	if m.height < pillsHeightReasonableTerminalHeight {
		return nil
	}
	hasPills := hasIncompleteTodos(m.session.Todos) || m.promptQueue > 0
	if !hasPills {
		return nil
	}
	if m.pillsExpanded {
		return nil
	}
	if m.pillsAutoExpanded {
		return nil
	}
	m.pillsExpanded = true
	m.pillsAutoExpanded = true
	if hasIncompleteTodos(m.session.Todos) {
		m.focusedPillSection = pillSectionTodos
	} else {
		m.focusedPillSection = pillSectionQueue
	}
	m.updateLayoutAndSize()
	if m.chat.Follow() {
		m.chat.ScrollToBottom()
	}
	return nil
}

// togglePillsExpanded toggles the pills panel expansion state.
func (m *UI) togglePillsExpanded() tea.Cmd {
	if !m.hasSession() {
		return nil
	}
	hasPills := hasIncompleteTodos(m.session.Todos) || m.promptQueue > 0
	if !hasPills {
		return nil
	}
	m.pillsExpanded = !m.pillsExpanded
	if m.pillsExpanded {
		if hasIncompleteTodos(m.session.Todos) {
			m.focusedPillSection = pillSectionTodos
		} else {
			m.focusedPillSection = pillSectionQueue
		}
	}
	m.updateLayoutAndSize()

	// Make sure to follow scroll if follow is enabled when toggling pills.
	// Note: uses ScrollToBottom (no scrollbar) since this is layout adjustment,
	// not user-initiated scrolling.
	if m.chat.Follow() {
		m.chat.ScrollToBottom()
	}

	return nil
}

// switchPillSection changes focus between todo and queue sections.
func (m *UI) switchPillSection(dir int) tea.Cmd {
	if !m.pillsExpanded || !m.hasSession() {
		return nil
	}
	hasIncompleteTodos := hasIncompleteTodos(m.session.Todos)
	hasQueue := m.promptQueue > 0

	if dir < 0 && m.focusedPillSection == pillSectionQueue && hasIncompleteTodos {
		m.focusedPillSection = pillSectionTodos
		m.updateLayoutAndSize()
		return nil
	}
	if dir > 0 && m.focusedPillSection == pillSectionTodos && hasQueue {
		m.focusedPillSection = pillSectionQueue
		m.updateLayoutAndSize()
		return nil
	}
	return nil
}

// effectiveFocusedSection returns the pill section that should be treated as
// focused for rendering. The stored focusedPillSection can go stale when its
// section loses all content (for example todos complete while the panel is open,
// or it defaults to todos before any todos exist). In that case we fall through
// to whichever section still has content so the expanded list stays populated.
func (m *UI) effectiveFocusedSection() pillSection {
	hasIncomplete := hasIncompleteTodos(m.session.Todos)
	hasQueue := m.promptQueue > 0
	switch m.focusedPillSection {
	case pillSectionQueue:
		if hasQueue {
			return pillSectionQueue
		}
		if hasIncomplete {
			return pillSectionTodos
		}
	default: // pillSectionTodos
		if hasIncomplete {
			return pillSectionTodos
		}
		if hasQueue {
			return pillSectionQueue
		}
	}
	return m.focusedPillSection
}

// pillsAreaHeight calculates the total height needed for the pills area.
func (m *UI) pillsAreaHeight() int {
	if m.taskPanelVisible() {
		if lines := m.taskPanel.PanelInfoLines(); len(lines) > 0 {
			return len(lines) + 1
		}
	}
	if !m.hasSession() {
		return 0
	}
	// Suppress pills when an inline editor (e.g. question form) is active
	// to avoid competing for screen space.
	if m.activeInline != nil {
		return 0
	}
	hasIncomplete := hasIncompleteTodos(m.session.Todos)
	hasQueue := m.promptQueue > 0
	hasPills := hasIncomplete || hasQueue
	if !hasPills {
		return 0
	}

	pillsAreaHeight := pillHeightWithBorder
	if m.pillsExpanded {
		switch m.effectiveFocusedSection() {
		case pillSectionTodos:
			if hasIncomplete {
				pillsAreaHeight = 3 + min(4, len(m.session.Todos))
				if hasQueue {
					pillsAreaHeight += pillHeightWithBorder
				}
			}
		case pillSectionQueue:
			if hasQueue {
				pillsAreaHeight += m.promptQueue
			}
		}
	}
	return pillsAreaHeight
}

// renderPills renders the pills panel and stores it in m.pillsView.
func (m *UI) renderPills() {
	m.pillsView = ""
	if m.taskPanelVisible() {
		if lines := m.taskPanel.PanelInfoLines(); len(lines) > 0 {
			m.pillsView = m.taskPanel.RenderPanelInfo(m.layout.pills.Dx(), m.editorAccent())
			return
		}
	}
	if !m.hasSession() {
		return
	}
	// Suppress pills when an inline editor (e.g. question form) is active.
	if m.activeInline != nil {
		return
	}

	width := m.layout.pills.Dx()
	if width <= 0 {
		return
	}

	paddingLeft := 3
	contentWidth := max(width-paddingLeft, 0)

	hasIncomplete := hasIncompleteTodos(m.session.Todos)
	hasQueue := m.promptQueue > 0

	if !hasIncomplete && !hasQueue {
		return
	}

	t := m.com.Styles
	effective := m.effectiveFocusedSection()
	todosFocused := m.pillsExpanded && effective == pillSectionTodos
	queueFocused := m.pillsExpanded && effective == pillSectionQueue

	inProgressIcon := t.Tool.TodoInProgressIcon.Render(styles.SpinnerIcon)
	if m.todoIsSpinning {
		inProgressIcon = m.todoSpinner.View()
	}

	if todosFocused && hasIncomplete && width >= 4 {
		background := t.Background
		frame := lipgloss.NewStyle().Foreground(m.editorAccent()).Background(background)
		completed := 0
		for _, todo := range m.session.Todos {
			if todo.Status == session.TodoStatusCompleted {
				completed++
			}
		}
		label := fmt.Sprintf(" To-Do %d/%d  ctrl+t close ", completed, len(m.session.Todos))
		list := todoList(m.session.Todos, inProgressIcon, t, width-4)
		contentWidth := 0
		for _, line := range strings.Split(list, "\n") {
			contentWidth = max(contentWidth, ansi.StringWidth(strings.TrimRight(ansi.Strip(line), " ")))
		}
		width = min(width, max(contentWidth+4, ansi.StringWidth(label)+2))
		label = ansi.Truncate(label, width-2, "…")
		rows := []string{frame.Render("╭" + label + strings.Repeat("─", max(0, width-2-ansi.StringWidth(label))) + "╮")}
		list = todoList(m.session.Todos, inProgressIcon, t, width-4)
		for _, line := range strings.Split(list, "\n") {
			body := paintEditorBody(" "+line+" ", width-2, background)
			rows = append(rows, frame.Render("│")+body+frame.Render("│"))
		}
		rows = append(rows, frame.Render("╰"+strings.Repeat("─", width-2)+"╯"), strings.Repeat(" ", width))
		m.pillsView = strings.Join(rows, "\n")
		if hasQueue {
			m.pillsView = lipgloss.JoinVertical(lipgloss.Left, queuePill(m.promptQueue, t, m.promptQueueItems...), m.pillsView)
		}
		return
	}

	var pills []string
	if hasIncomplete {
		pills = append(pills, todoPill(m.session.Todos, inProgressIcon, m.pillsExpanded, t))
	}
	if hasQueue {
		pills = append(pills, queuePill(m.promptQueue, t, m.promptQueueItems...))
	}

	var expandedList string
	if m.pillsExpanded {
		if todosFocused && hasIncomplete {
			expandedList = todoList(m.session.Todos, inProgressIcon, t, contentWidth)
		} else if queueFocused && hasQueue {
			// Render from the memoized queue (fetched off-thread, see
			// workspace_cache.go): renderPills runs on the Update/View
			// path and must never block on a workspace round-trip.
			if len(m.promptQueueItems) > 0 {
				expandedList = queueList(m.promptQueueItems, t)
			}
		}
	}

	if len(pills) == 0 {
		return
	}

	pillsRow := lipgloss.JoinHorizontal(lipgloss.Top, pills...)

	helpDesc := "open"
	if m.pillsExpanded {
		helpDesc = "close"
	}
	helpKey := t.Pills.HelpKey.Render("ctrl+t")
	helpText := t.Pills.HelpText.Render(helpDesc)
	helpHint := lipgloss.JoinHorizontal(lipgloss.Center, helpKey, " ", helpText)
	pillsRow = lipgloss.JoinHorizontal(lipgloss.Center, pillsRow, " ", helpHint)

	pillsArea := pillsRow
	if expandedList != "" {
		pillsArea = lipgloss.JoinVertical(lipgloss.Left, pillsRow, expandedList)
	}

	m.pillsView = t.Pills.Area.MaxWidth(width).PaddingLeft(paddingLeft).Render(pillsArea)
}
