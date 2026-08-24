package servermenu

import (
	"context"
	"fmt"
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/example-git/crux/internal/proto"
)

type Client interface {
	RefreshWorkspaces(context.Context) ([]proto.Workspace, error)
	Browse(context.Context, string) (proto.BrowserListing, error)
	CloseIdleWorkspace(context.Context, string) error
}

type Selection struct {
	WorkspaceID string
	Path        string
}

type Model struct {
	ctx        context.Context
	client     Client
	workspaces []proto.Workspace
	browser    proto.BrowserListing
	pane       int
	workspace  int
	entry      int
	width      int
	height     int
	loading    int
	errorText  string
	confirmID  string
	selection  Selection
	quitting   bool
}

type workspacesMsg struct {
	workspaces []proto.Workspace
	err        error
}

type browserMsg struct {
	listing proto.BrowserListing
	err     error
}

type closedMsg struct {
	err error
}

func New(ctx context.Context, client Client) *Model {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Model{ctx: ctx, client: client, width: 100, height: 30}
}

func (m *Model) Init() tea.Cmd {
	m.loading = 2
	return tea.Batch(m.loadWorkspaces(), m.loadBrowser(""))
}

func (m *Model) Selection() Selection {
	return m.selection
}

func (m *Model) SetError(err error) {
	if err != nil {
		m.errorText = err.Error()
	}
}

func (m *Model) Quitting() bool {
	return m.quitting
}

func (m *Model) loadWorkspaces() tea.Cmd {
	return func() tea.Msg {
		workspaces, err := m.client.RefreshWorkspaces(m.ctx)
		return workspacesMsg{workspaces: workspaces, err: err}
	}
}

func (m *Model) loadBrowser(path string) tea.Cmd {
	return func() tea.Msg {
		listing, err := m.client.Browse(m.ctx, path)
		return browserMsg{listing: listing, err: err}
	}
}

func (m *Model) closeWorkspace(id string) tea.Cmd {
	return func() tea.Msg {
		return closedMsg{err: m.client.CloseIdleWorkspace(m.ctx, id)}
	}
}

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
	case workspacesMsg:
		if m.loading > 0 {
			m.loading--
		}
		if message.err != nil {
			m.errorText = message.err.Error()
		} else {
			m.workspaces = message.workspaces
			m.workspace = clamp(m.workspace, len(m.workspaces))
		}
	case browserMsg:
		if m.loading > 0 {
			m.loading--
		}
		if message.err != nil {
			m.errorText = message.err.Error()
		} else {
			m.browser = message.listing
			m.entry = 0
		}
	case closedMsg:
		m.confirmID = ""
		if message.err != nil {
			m.errorText = message.err.Error()
			return m, nil
		}
		m.loading++
		return m, m.loadWorkspaces()
	case tea.KeyPressMsg:
		key := message.String()
		if m.confirmID != "" {
			switch key {
			case "y", "enter":
				id := m.confirmID
				m.loading++
				return m, m.closeWorkspace(id)
			case "n", "esc", "q":
				m.confirmID = ""
			}
			return m, nil
		}
		switch key {
		case "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "tab":
			m.pane = (m.pane + 1) % 2
		case "up", "k":
			m.move(-1)
		case "down", "j":
			m.move(1)
		case "enter":
			if m.pane == 0 && len(m.workspaces) > 0 {
				m.selection.WorkspaceID = m.workspaces[m.workspace].ID
				return m, tea.Quit
			}
			if m.pane == 1 && len(m.browser.Entries) > 0 && m.browser.Entries[m.entry].Directory {
				m.loading++
				return m, m.loadBrowser(m.browser.Entries[m.entry].Path)
			}
		case "o":
			if m.browser.Path != "" {
				m.selection.Path = m.browser.Path
				return m, tea.Quit
			}
		case "left", "backspace":
			if m.pane == 1 && m.browser.Parent != "" {
				m.loading++
				return m, m.loadBrowser(m.browser.Parent)
			}
		case "r":
			m.errorText = ""
			m.loading += 2
			return m, tea.Batch(m.loadWorkspaces(), m.loadBrowser(m.browser.Path))
		case "d":
			if m.pane == 0 && len(m.workspaces) > 0 {
				m.confirmID = m.workspaces[m.workspace].ID
			}
		}
	}
	return m, nil
}

func (m *Model) move(delta int) {
	if m.pane == 0 {
		m.workspace = wrap(m.workspace+delta, len(m.workspaces))
		return
	}
	m.entry = wrap(m.entry+delta, len(m.browser.Entries))
}

func (m *Model) View() tea.View {
	green := lipgloss.Color("#39FF14")
	orange := lipgloss.Color("#FF3B1F")
	muted := lipgloss.Color("#777777")
	title := lipgloss.NewStyle().Bold(true).Foreground(green).Render("CRUX SERVER")
	status := "Tab switch  Enter open  o open current  r refresh  d close  q quit"
	if m.loading > 0 {
		status = "Loading server state..."
	}
	if m.errorText != "" {
		status = lipgloss.NewStyle().Foreground(orange).Render("Error: " + oneLine(m.errorText, max(20, m.width-10)))
	}
	availableHeight := max(5, m.height-7)
	workspaces := m.renderWorkspaces(availableHeight)
	browser := m.renderBrowser(availableHeight)
	var panes string
	if m.width >= 72 {
		leftWidth := max(28, m.width/2-2)
		rightWidth := max(28, m.width-leftWidth-3)
		panes = lipgloss.JoinHorizontal(lipgloss.Top, m.paneStyle(0, green, orange, leftWidth, availableHeight).Render(workspaces), " ", m.paneStyle(1, green, orange, rightWidth, availableHeight).Render(browser))
	} else {
		paneHeight := max(4, availableHeight/2)
		panes = lipgloss.JoinVertical(lipgloss.Left, m.paneStyle(0, green, orange, max(20, m.width-2), paneHeight).Render(workspaces), m.paneStyle(1, green, orange, max(20, m.width-2), paneHeight).Render(browser))
	}
	footer := lipgloss.NewStyle().Foreground(muted).Render(status)
	if m.confirmID != "" {
		footer = lipgloss.NewStyle().Bold(true).Foreground(orange).Render("Close this idle workspace? y/Enter confirm, n/Esc cancel")
	}
	view := tea.NewView(strings.Join([]string{title, panes, footer}, "\n"))
	view.AltScreen = true
	view.WindowTitle = "Crux server workspaces"
	return view
}

func (m *Model) paneStyle(pane int, green, orange color.Color, width, height int) lipgloss.Style {
	color := green
	if m.pane == pane {
		color = orange
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color).Width(width).Height(height)
}

func (m *Model) renderWorkspaces(height int) string {
	lines := []string{lipgloss.NewStyle().Bold(true).Render("Workspaces")}
	if len(m.workspaces) == 0 {
		lines = append(lines, "No server workspaces")
		return strings.Join(lines, "\n")
	}
	limit := max(1, height-2)
	start := visibleStart(m.workspace, len(m.workspaces), limit)
	for index := start; index < len(m.workspaces) && index < start+limit; index++ {
		workspace := m.workspaces[index]
		marker := "  "
		if m.pane == 0 && index == m.workspace {
			marker = "> "
		}
		state := "idle"
		if workspace.ConnectedClients > 0 {
			state = fmt.Sprintf("%d connected", workspace.ConnectedClients)
		}
		lines = append(lines, fmt.Sprintf("%s%s  [%s]", marker, oneLine(workspace.Path, 48), state))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderBrowser(height int) string {
	lines := []string{lipgloss.NewStyle().Bold(true).Render("Server files"), oneLine(m.browser.Path, 56)}
	if len(m.browser.Entries) == 0 {
		lines = append(lines, "No entries")
		return strings.Join(lines, "\n")
	}
	limit := max(1, height-3)
	start := visibleStart(m.entry, len(m.browser.Entries), limit)
	for index := start; index < len(m.browser.Entries) && index < start+limit; index++ {
		entry := m.browser.Entries[index]
		marker := "  "
		if m.pane == 1 && index == m.entry {
			marker = "> "
		}
		name := entry.Name
		if entry.Directory {
			name += "/"
		}
		lines = append(lines, marker+oneLine(name, 52))
	}
	if m.browser.Truncated {
		lines = append(lines, "... listing truncated")
	}
	return strings.Join(lines, "\n")
}

func visibleStart(selected, length, limit int) int {
	if length <= limit || selected < limit {
		return 0
	}
	start := selected - limit + 1
	if start+limit > length {
		start = length - limit
	}
	return start
}

func oneLine(value string, limit int) string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\r", " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}

func clamp(value, length int) int {
	if length == 0 {
		return 0
	}
	if value >= length {
		return length - 1
	}
	return max(0, value)
}

func wrap(value, length int) int {
	if length == 0 {
		return 0
	}
	if value < 0 {
		return length - 1
	}
	return value % length
}
