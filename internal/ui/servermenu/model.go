package servermenu

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"unicode/utf8"

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
	ctx               context.Context
	client            Client
	connectionName    string
	connectionAddr    string
	workspaces        []proto.Workspace
	browser           proto.BrowserListing
	pane              int
	workspace         int
	entry             int
	width             int
	height            int
	workspaceLoading  bool
	browserLoading    bool
	workspaceError    string
	browserError      string
	statusText        string
	confirmID         string
	confirmPath       string
	filtering         bool
	filter            string
	browserSelections map[string]string
	selection         Selection
	quitting          bool
}

type workspacesMsg struct {
	workspaces []proto.Workspace
	selectedID string
	err        error
}

type browserMsg struct {
	listing      proto.BrowserListing
	selectedPath string
	err          error
}

type closedMsg struct {
	err error
}

func New(ctx context.Context, client Client) *Model {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Model{ctx: ctx, client: client, width: 100, height: 30, browserSelections: make(map[string]string)}
}

func (m *Model) SetConnection(name, address string) {
	m.connectionName = name
	m.connectionAddr = address
}

func (m *Model) Init() tea.Cmd {
	m.workspaceLoading = true
	m.browserLoading = true
	return tea.Batch(m.loadWorkspaces(""), m.loadBrowser("", ""))
}

func (m *Model) Selection() Selection {
	return m.selection
}

func (m *Model) SetError(err error) {
	if err != nil {
		m.statusText = err.Error()
	}
}

func (m *Model) Quitting() bool {
	return m.quitting
}

func (m *Model) loadWorkspaces(selectedID string) tea.Cmd {
	return func() tea.Msg {
		workspaces, err := m.client.RefreshWorkspaces(m.ctx)
		return workspacesMsg{workspaces: workspaces, selectedID: selectedID, err: err}
	}
}

func (m *Model) loadBrowser(path, selectedPath string) tea.Cmd {
	return func() tea.Msg {
		listing, err := m.client.Browse(m.ctx, path)
		return browserMsg{listing: listing, selectedPath: selectedPath, err: err}
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
		m.workspaceLoading = false
		if message.err != nil {
			m.workspaceError = message.err.Error()
		} else {
			m.workspaceError = ""
			m.workspaces = message.workspaces
			m.selectWorkspace(message.selectedID)
		}
	case browserMsg:
		m.browserLoading = false
		if message.err != nil {
			m.browserError = message.err.Error()
		} else {
			m.browserError = ""
			m.browser = message.listing
			m.filter = ""
			m.filtering = false
			selectedPath := message.selectedPath
			if selectedPath == "" {
				selectedPath = m.browserSelections[m.browser.Path]
			}
			m.selectBrowserPath(selectedPath)
		}
	case closedMsg:
		m.confirmID = ""
		m.confirmPath = ""
		if message.err != nil {
			m.workspaceError = message.err.Error()
			return m, nil
		}
		m.workspaceLoading = true
		m.statusText = "Workspace closed."
		return m, m.loadWorkspaces("")
	case tea.KeyPressMsg:
		if m.confirmID != "" {
			return m.handleConfirmation(message.String())
		}
		if m.filtering {
			return m.handleFilter(message)
		}
		return m.handleKey(message)
	}
	return m, nil
}

func (m *Model) handleConfirmation(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "y", "enter":
		id := m.confirmID
		m.workspaceLoading = true
		return m, m.closeWorkspace(id)
	case "n", "esc", "q":
		m.confirmID = ""
		m.confirmPath = ""
	}
	return m, nil
}

func (m *Model) handleFilter(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch message.String() {
	case "esc":
		m.filtering = false
		m.filter = ""
		m.entry = 0
	case "enter":
		m.filtering = false
	case "backspace":
		if m.filter != "" {
			_, size := utf8.DecodeLastRuneInString(m.filter)
			m.filter = m.filter[:len(m.filter)-size]
			m.entry = 0
		}
	default:
		if message.Text != "" && !strings.ContainsAny(message.Text, "\r\n") {
			m.filter += message.Text
			m.entry = 0
		}
	}
	return m, nil
}

func (m *Model) handleKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	switch key {
	case "q", "esc":
		m.quitting = true
		return m, tea.Quit
	case "tab", "shift+tab":
		m.pane = (m.pane + 1) % 2
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "enter":
		if m.pane == 0 {
			if len(m.workspaces) > 0 {
				m.selection.WorkspaceID = m.workspaces[m.workspace].ID
				return m, tea.Quit
			}
			return m, nil
		}
		entry, ok := m.selectedBrowserEntry()
		if !ok || !entry.Directory {
			m.statusText = "Select a directory to browse it. Press o to open the current directory as a workspace."
			return m, nil
		}
		m.rememberBrowserSelection()
		m.browserLoading = true
		return m, m.loadBrowser(entry.Path, "")
	case "o":
		if m.pane != 1 || m.browser.Path == "" || m.browserLoading {
			m.statusText = "Opening a workspace is unavailable until a server directory is loaded."
			return m, nil
		}
		m.selection.Path = m.browser.Path
		return m, tea.Quit
	case "left", "backspace":
		if m.pane == 1 && m.browser.Parent != "" && !m.browserLoading {
			currentPath := m.browser.Path
			m.rememberBrowserSelection()
			m.browserLoading = true
			return m, m.loadBrowser(m.browser.Parent, currentPath)
		}
	case "[":
		return m.switchRoot(-1)
	case "]":
		return m.switchRoot(1)
	case "/":
		if m.pane == 1 && len(m.browser.Entries) > 0 {
			m.filtering = true
			m.filter = ""
			m.entry = 0
		}
	case "r":
		m.statusText = ""
		if m.pane == 0 {
			selectedID := m.selectedWorkspaceID()
			m.workspaceLoading = true
			return m, m.loadWorkspaces(selectedID)
		}
		selectedPath := m.selectedBrowserPath()
		m.browserLoading = true
		return m, m.loadBrowser(m.browser.Path, selectedPath)
	case "R", "ctrl+r":
		m.statusText = ""
		m.workspaceLoading = true
		m.browserLoading = true
		return m, tea.Batch(m.loadWorkspaces(m.selectedWorkspaceID()), m.loadBrowser(m.browser.Path, m.selectedBrowserPath()))
	case "d":
		if m.pane == 0 && len(m.workspaces) > 0 {
			workspace := m.workspaces[m.workspace]
			if workspace.ConnectedClients > 0 {
				m.statusText = "Connected workspaces cannot be closed."
				return m, nil
			}
			m.confirmID = workspace.ID
			m.confirmPath = workspace.Path
		}
	}
	return m, nil
}

func (m *Model) switchRoot(delta int) (tea.Model, tea.Cmd) {
	if m.pane != 1 || len(m.browser.Roots) == 0 || m.browserLoading {
		return m, nil
	}
	current := 0
	bestLength := -1
	for index, root := range m.browser.Roots {
		if pathWithin(m.browser.Path, root) && len(root) > bestLength {
			current = index
			bestLength = len(root)
		}
	}
	next := wrap(current+delta, len(m.browser.Roots))
	m.rememberBrowserSelection()
	m.browserLoading = true
	return m, m.loadBrowser(m.browser.Roots[next], "")
}

func (m *Model) move(delta int) {
	if m.pane == 0 {
		m.workspace = wrap(m.workspace+delta, len(m.workspaces))
		return
	}
	m.entry = wrap(m.entry+delta, len(m.filteredEntries()))
}

func (m *Model) selectWorkspace(id string) {
	if id != "" {
		for index := range m.workspaces {
			if m.workspaces[index].ID == id {
				m.workspace = index
				return
			}
		}
	}
	m.workspace = clamp(m.workspace, len(m.workspaces))
}

func (m *Model) selectedWorkspaceID() string {
	if len(m.workspaces) == 0 {
		return ""
	}
	return m.workspaces[m.workspace].ID
}

func (m *Model) filteredEntries() []proto.BrowserEntry {
	if m.filter == "" {
		return m.browser.Entries
	}
	needle := strings.ToLower(m.filter)
	entries := make([]proto.BrowserEntry, 0, len(m.browser.Entries))
	for _, entry := range m.browser.Entries {
		if strings.Contains(strings.ToLower(entry.Name), needle) {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (m *Model) selectedBrowserEntry() (proto.BrowserEntry, bool) {
	entries := m.filteredEntries()
	if len(entries) == 0 {
		return proto.BrowserEntry{}, false
	}
	m.entry = clamp(m.entry, len(entries))
	return entries[m.entry], true
}

func (m *Model) selectedBrowserPath() string {
	entry, ok := m.selectedBrowserEntry()
	if !ok {
		return ""
	}
	return entry.Path
}

func (m *Model) rememberBrowserSelection() {
	if m.browser.Path == "" {
		return
	}
	if selected := m.selectedBrowserPath(); selected != "" {
		m.browserSelections[m.browser.Path] = selected
	}
}

func (m *Model) selectBrowserPath(path string) {
	entries := m.filteredEntries()
	if path != "" {
		for index := range entries {
			if entries[index].Path == path {
				m.entry = index
				return
			}
		}
	}
	m.entry = clamp(m.entry, len(entries))
}

func (m *Model) View() tea.View {
	green := lipgloss.Color("#39FF14")
	orange := lipgloss.Color("#FF7A1A")
	muted := lipgloss.Color("#777777")
	title := lipgloss.NewStyle().Bold(true).Foreground(green).Render("CRUX SERVER")
	identity := strings.TrimSpace(strings.Join([]string{m.connectionName, m.connectionAddr}, "  "))
	if identity != "" {
		title += "  " + lipgloss.NewStyle().Foreground(muted).Render(oneLine(identity, max(20, m.width-18)))
	}
	availableHeight := max(5, m.height-8)
	workspaces := m.renderWorkspaces(availableHeight)
	browser := m.renderBrowser(availableHeight)
	var panes string
	if m.width >= 72 {
		leftWidth := max(28, m.width/2-2)
		rightWidth := max(28, m.width-leftWidth-3)
		panes = lipgloss.JoinHorizontal(lipgloss.Top, m.paneStyle(0, green, muted, leftWidth, availableHeight).Render(workspaces), " ", m.paneStyle(1, green, muted, rightWidth, availableHeight).Render(browser))
	} else {
		paneHeight := max(4, availableHeight/2)
		panes = lipgloss.JoinVertical(lipgloss.Left, m.paneStyle(0, green, muted, max(20, m.width-2), paneHeight).Render(workspaces), m.paneStyle(1, green, muted, max(20, m.width-2), paneHeight).Render(browser))
	}
	status := m.statusLine()
	footer := lipgloss.NewStyle().Foreground(muted).Render(oneLine(status, max(20, m.width-2)))
	if m.workspaceError != "" || m.browserError != "" {
		errors := make([]string, 0, 2)
		if m.workspaceError != "" {
			errors = append(errors, "workspaces: "+m.workspaceError)
		}
		if m.browserError != "" {
			errors = append(errors, "browser: "+m.browserError)
		}
		footer = lipgloss.NewStyle().Foreground(orange).Render("Error: " + oneLine(strings.Join(errors, "; "), max(20, m.width-10)))
	}
	if m.confirmID != "" {
		footer = lipgloss.NewStyle().Bold(true).Foreground(orange).Render("Close idle workspace " + oneLine(m.confirmPath, max(12, m.width-55)) + "? y/Enter confirm, n/Esc cancel")
	}
	view := tea.NewView(strings.Join([]string{title, panes, footer}, "\n"))
	view.AltScreen = true
	view.WindowTitle = "Crux server workspaces"
	return view
}

func (m *Model) statusLine() string {
	if m.filtering {
		return "Filter: " + m.filter + "_  Enter apply  Esc clear"
	}
	if m.statusText != "" {
		return m.statusText
	}
	if m.workspaceLoading || m.browserLoading {
		parts := make([]string, 0, 2)
		if m.workspaceLoading {
			parts = append(parts, "workspaces")
		}
		if m.browserLoading {
			parts = append(parts, "files")
		}
		return "Loading " + strings.Join(parts, " and ") + "..."
	}
	if m.pane == 0 {
		return "Enter connect  d close idle  r refresh  R refresh all  Tab files  q quit"
	}
	return "Enter browse  ←/Backspace parent  [/] roots  / filter  o open workspace  r refresh  Tab workspaces"
}

func (m *Model) paneStyle(pane int, active, inactive color.Color, width, height int) lipgloss.Style {
	borderColor := inactive
	if m.pane == pane {
		borderColor = active
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(borderColor).Width(width).Height(height)
}

func (m *Model) renderWorkspaces(height int) string {
	heading := "Workspaces"
	if m.workspaceLoading {
		heading += "  loading..."
	}
	lines := []string{lipgloss.NewStyle().Bold(true).Render(heading)}
	if len(m.workspaces) == 0 {
		lines = append(lines, "No server workspaces.", "Use the Files pane to open a directory.")
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
		state := "idle, closable"
		if workspace.ConnectedClients > 0 {
			state = fmt.Sprintf("connected: %d", workspace.ConnectedClients)
		}
		lines = append(lines, fmt.Sprintf("%s%s  [%s]", marker, oneLine(workspace.Path, 48), state))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderBrowser(height int) string {
	heading := "Server files"
	if m.browserLoading {
		heading += "  loading..."
	}
	root := m.activeRoot()
	lines := []string{lipgloss.NewStyle().Bold(true).Render(heading)}
	if root != "" {
		lines = append(lines, "Root: "+oneLine(root, 50))
	}
	lines = append(lines, "Path: "+oneLine(m.browser.Path, 50))
	entries := m.filteredEntries()
	if len(entries) == 0 {
		if m.filter != "" {
			lines = append(lines, "No entries match “"+oneLine(m.filter, 30)+"”.")
		} else {
			lines = append(lines, "This directory is empty.", "Press o to open it as a workspace.")
		}
		return strings.Join(lines, "\n")
	}
	limit := max(1, height-4)
	start := visibleStart(m.entry, len(entries), limit)
	for index := start; index < len(entries) && index < start+limit; index++ {
		entry := entries[index]
		marker := "  "
		if m.pane == 1 && index == m.entry {
			marker = "> "
		}
		kind := "f "
		name := entry.Name
		if entry.Directory {
			kind = "d "
			name += "/"
		}
		lines = append(lines, marker+kind+oneLine(name, 48))
	}
	if m.browser.Truncated {
		lines = append(lines, "Warning: listing truncated by the server.")
	}
	return strings.Join(lines, "\n")
}

func (m *Model) activeRoot() string {
	best := ""
	for _, root := range m.browser.Roots {
		if pathWithin(m.browser.Path, root) && len(root) > len(best) {
			best = root
		}
	}
	return best
}

func pathWithin(path, root string) bool {
	if path == root {
		return true
	}
	if root == "" || !strings.HasPrefix(path, root) || len(path) <= len(root) {
		return false
	}
	separator := path[len(root)]
	return separator == '/' || separator == '\\'
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
