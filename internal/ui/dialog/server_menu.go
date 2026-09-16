package dialog

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/foundation/bubbles/help"
	"github.com/example-git/crux/foundation/bubbles/key"
	"github.com/example-git/crux/foundation/bubbles/textinput"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/ui/styles"
)

// ServerMenuID is the identifier for the server workspace menu dialog.
const ServerMenuID = "server-menu"

// ServerMenuClient is the transport the [ServerMenu] dialog needs from an
// authenticated remote connection: listing/browsing/closing workspaces and
// creating new project directories under the server's configured roots.
// Implementations should prefer the connection-scoped peer channel (for
// push-based refresh and progress streaming) and fall back to plain HTTP
// only when the peer channel is unavailable; that choice is the caller's,
// not this dialog's.
type ServerMenuClient interface {
	RefreshWorkspaces(ctx context.Context) ([]proto.Workspace, error)
	Browse(ctx context.Context, path string) (proto.BrowserListing, error)
	CloseIdleWorkspace(ctx context.Context, id string) error
	CreateProject(ctx context.Context, request proto.PeerWorkspaceCreateRequest, progress func(string)) (string, error)
}

// ServerMenuSelection is the terminal result of the server menu: either an
// existing workspace ID to reattach to, or a server-side path to open (and
// implicitly create, if it was just created via the New Project form) as a
// workspace.
type ServerMenuSelection struct {
	WorkspaceID string
	Path        string
}

// ActionServerMenuSelected is returned when the user picks an existing
// workspace, a browsed directory, or a freshly created project to open.
type ActionServerMenuSelected struct {
	Selection ServerMenuSelection
}

type serverMenuMode int

const (
	serverMenuWorkspaces serverMenuMode = iota
	serverMenuBrowser
	serverMenuNewProject
)

type projectCreateMode int

const (
	projectModePlain projectCreateMode = iota
	projectModeGitInit
	projectModeClone
)

func (m projectCreateMode) label() string {
	switch m {
	case projectModeGitInit:
		return "mkdir + git init"
	case projectModeClone:
		return "mkdir + git clone <url>"
	default:
		return "mkdir only"
	}
}

func (m projectCreateMode) wire() proto.PeerWorkspaceCreateMode {
	switch m {
	case projectModeGitInit:
		return proto.PeerWorkspaceCreateGitInit
	case projectModeClone:
		return proto.PeerWorkspaceCreateClone
	default:
		return proto.PeerWorkspaceCreatePlain
	}
}

// New Project form field indices.
const (
	projectFieldName int = iota
	projectFieldMode
	projectFieldURL
	projectFieldSubmit
)

// ServerMenu lets the user reattach to an existing server-hosted workspace,
// browse the server's configured workspace roots to open a new one, or
// create a new project directory (optionally via `git init`/`git clone`)
// rooted under one of those roots.
type ServerMenu struct {
	ctx    context.Context
	client ServerMenuClient
	styles *styles.Styles
	help   help.Model

	connectionName string
	connectionAddr string

	mode serverMenuMode

	workspaces       []proto.Workspace
	browser          proto.BrowserListing
	workspaceCursor  int
	browserCursor    int
	workspaceLoading bool
	browserLoading   bool
	workspaceError   string
	browserError     string
	statusText       string

	confirmID   string
	confirmPath string

	filtering bool
	filter    string

	browserSelections map[string]string

	projectField   int
	projectName    textinput.Model
	projectURL     textinput.Model
	projectMode    projectCreateMode
	projectBusy    bool
	projectError   string
	projectLines   []string
	returnFromForm serverMenuMode

	keyMap serverMenuKeyMap
}

type serverMenuKeyMap struct {
	Up      key.Binding
	Down    key.Binding
	LeftMod key.Binding
	Enter   key.Binding
	Tab     key.Binding
	Open    key.Binding
	New     key.Binding
	Filter  key.Binding
	Refresh key.Binding
	Delete  key.Binding
	Close   key.Binding
}

var _ Dialog = (*ServerMenu)(nil)

// NewServerMenu builds the server workspace menu dialog and returns the
// command that kicks off the initial workspace/browser loads.
func NewServerMenu(ctx context.Context, client ServerMenuClient, connectionName, connectionAddr string) (*ServerMenu, tea.Cmd) {
	if ctx == nil {
		ctx = context.Background()
	}
	t := styles.ThemeForProvider("")
	d := &ServerMenu{
		ctx:               ctx,
		client:            client,
		styles:            &t,
		connectionName:    connectionName,
		connectionAddr:    connectionAddr,
		browserSelections: make(map[string]string),
	}
	d.help = help.New()
	d.help.Styles = d.styles.DialogHelpStyles()
	d.keyMap = serverMenuKeyMap{
		Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		LeftMod: key.NewBinding(key.WithKeys("left", "right"), key.WithHelp("←/→", "change")),
		Enter:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select")),
		Tab:     key.NewBinding(key.WithKeys("tab", "shift+tab"), key.WithHelp("tab", "switch pane")),
		Open:    key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open dir")),
		New:     key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new project")),
		Filter:  key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Delete:  key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "close idle")),
		Close:   CloseKey,
	}
	d.projectName = d.newProjectInput("my-project")
	d.projectURL = d.newProjectInput("https://example.com/user/repo.git")
	d.workspaceLoading = true
	d.browserLoading = true
	return d, tea.Batch(d.loadWorkspaces(""), d.loadBrowser("", ""))
}

// SetStatusError displays a status-line error, for example one carried over
// from a previous failed attempt to open a selection returned by this
// dialog. It does not affect the workspace/browser loading state.
func (d *ServerMenu) SetStatusError(err error) {
	if err != nil {
		d.statusText = err.Error()
	}
}

func (d *ServerMenu) newProjectInput(placeholder string) textinput.Model {
	input := textinput.New()
	input.SetVirtualCursor(true)
	input.Placeholder = placeholder
	input.SetStyles(d.styles.TextInput)
	return input
}

// ID implements [Dialog].
func (*ServerMenu) ID() string { return ServerMenuID }

// Internal async-load result messages.
type serverMenuWorkspacesMsg struct {
	workspaces []proto.Workspace
	selectedID string
	err        error
}

type serverMenuBrowserMsg struct {
	listing      proto.BrowserListing
	selectedPath string
	err          error
}

type serverMenuClosedMsg struct {
	err error
}

type serverMenuProjectCreatedMsg struct {
	path string
	err  error
}

func (d *ServerMenu) loadWorkspaces(selectedID string) tea.Cmd {
	return func() tea.Msg {
		workspaces, err := d.client.RefreshWorkspaces(d.ctx)
		return serverMenuWorkspacesMsg{workspaces: workspaces, selectedID: selectedID, err: err}
	}
}

func (d *ServerMenu) loadBrowser(path, selectedPath string) tea.Cmd {
	return func() tea.Msg {
		listing, err := d.client.Browse(d.ctx, path)
		return serverMenuBrowserMsg{listing: listing, selectedPath: selectedPath, err: err}
	}
}

func (d *ServerMenu) closeWorkspace(id string) tea.Cmd {
	return func() tea.Msg {
		return serverMenuClosedMsg{err: d.client.CloseIdleWorkspace(d.ctx, id)}
	}
}

func (d *ServerMenu) createProject() tea.Cmd {
	root := d.activeRoot()
	if root == "" {
		root = d.browser.Path
	}
	request := proto.PeerWorkspaceCreateRequest{
		Root:         root,
		RelativePath: strings.TrimSpace(d.projectName.Value()),
		Mode:         d.projectMode.wire(),
		CloneURL:     strings.TrimSpace(d.projectURL.Value()),
	}
	return func() tea.Msg {
		path, err := d.client.CreateProject(d.ctx, request, func(line string) {
			// Best-effort progress; dropped if the dialog has moved on.
			d.projectLines = append(d.projectLines, line)
		})
		return serverMenuProjectCreatedMsg{path: path, err: err}
	}
}

// HandleMsg implements [Dialog].
func (d *ServerMenu) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case serverMenuWorkspacesMsg:
		d.workspaceLoading = false
		if msg.err != nil {
			d.workspaceError = msg.err.Error()
		} else {
			d.workspaceError = ""
			d.workspaces = msg.workspaces
			d.selectWorkspace(msg.selectedID)
		}
		return nil
	case serverMenuBrowserMsg:
		d.browserLoading = false
		if msg.err != nil {
			d.browserError = msg.err.Error()
		} else {
			d.browserError = ""
			d.browser = msg.listing
			d.filter = ""
			d.filtering = false
			selectedPath := msg.selectedPath
			if selectedPath == "" {
				selectedPath = d.browserSelections[d.browser.Path]
			}
			d.selectBrowserPath(selectedPath)
		}
		return nil
	case serverMenuClosedMsg:
		d.confirmID = ""
		d.confirmPath = ""
		if msg.err != nil {
			d.workspaceLoading = false
			d.workspaceError = msg.err.Error()
			return nil
		}
		d.workspaceLoading = true
		d.statusText = "Workspace closed."
		return ActionCmd{Cmd: d.loadWorkspaces("")}
	case serverMenuProjectCreatedMsg:
		d.projectBusy = false
		if msg.err != nil {
			d.projectError = msg.err.Error()
			return nil
		}
		return ActionServerMenuSelected{Selection: ServerMenuSelection{Path: msg.path}}
	case tea.KeyPressMsg:
		return d.handleKey(msg)
	}
	return nil
}

func (d *ServerMenu) handleKey(msg tea.KeyPressMsg) Action {
	if d.confirmID != "" {
		return d.handleConfirmation(msg.String())
	}
	if d.mode == serverMenuNewProject {
		return d.handleProjectKey(msg)
	}
	if d.filtering {
		return d.handleFilterKey(msg)
	}
	switch {
	case key.Matches(msg, d.keyMap.Close):
		return ActionClose{}
	case key.Matches(msg, d.keyMap.Tab):
		if d.mode == serverMenuWorkspaces {
			d.mode = serverMenuBrowser
		} else {
			d.mode = serverMenuWorkspaces
		}
		return nil
	case key.Matches(msg, d.keyMap.Up):
		d.move(-1)
		return nil
	case key.Matches(msg, d.keyMap.Down):
		d.move(1)
		return nil
	case key.Matches(msg, d.keyMap.Enter):
		return d.handleEnter()
	case key.Matches(msg, d.keyMap.Open):
		if d.mode != serverMenuBrowser || d.browser.Path == "" || d.browserLoading {
			d.statusText = "Opening a workspace is unavailable until a server directory is loaded."
			return nil
		}
		return ActionServerMenuSelected{Selection: ServerMenuSelection{Path: d.browser.Path}}
	case key.Matches(msg, d.keyMap.New):
		if d.mode != serverMenuBrowser {
			return nil
		}
		d.openProjectForm()
		return nil
	case msg.String() == "left" || msg.String() == "right":
		return d.switchRoot(msg.String() == "right")
	case key.Matches(msg, d.keyMap.Filter):
		if d.mode == serverMenuBrowser && len(d.browser.Entries) > 0 {
			d.filtering = true
			d.filter = ""
			d.browserCursor = 0
		}
		return nil
	case key.Matches(msg, d.keyMap.Refresh):
		d.statusText = ""
		if d.mode == serverMenuWorkspaces {
			selectedID := d.selectedWorkspaceID()
			d.workspaceLoading = true
			return ActionCmd{Cmd: d.loadWorkspaces(selectedID)}
		}
		selectedPath := d.selectedBrowserPath()
		d.browserLoading = true
		return ActionCmd{Cmd: d.loadBrowser(d.browser.Path, selectedPath)}
	case key.Matches(msg, d.keyMap.Delete):
		if d.mode == serverMenuWorkspaces && len(d.workspaces) > 0 {
			workspace := d.workspaces[d.workspaceCursor]
			if workspace.ConnectedClients > 0 {
				d.statusText = "Connected workspaces cannot be closed."
				return nil
			}
			d.confirmID = workspace.ID
			d.confirmPath = workspace.Path
		}
		return nil
	case msg.String() == "backspace":
		if d.mode == serverMenuBrowser && d.browser.Parent != "" && !d.browserLoading {
			currentPath := d.browser.Path
			d.rememberBrowserSelection()
			d.browserLoading = true
			return ActionCmd{Cmd: d.loadBrowser(d.browser.Parent, currentPath)}
		}
		return nil
	}
	return nil
}

func (d *ServerMenu) handleEnter() Action {
	if d.mode == serverMenuWorkspaces {
		if len(d.workspaces) == 0 {
			return nil
		}
		return ActionServerMenuSelected{Selection: ServerMenuSelection{WorkspaceID: d.workspaces[d.workspaceCursor].ID}}
	}
	entry, ok := d.selectedBrowserEntry()
	if !ok || !entry.Directory {
		d.statusText = "Select a directory to browse it. Press o to open the current directory as a workspace."
		return nil
	}
	d.rememberBrowserSelection()
	d.browserLoading = true
	return ActionCmd{Cmd: d.loadBrowser(entry.Path, "")}
}

func (d *ServerMenu) handleConfirmation(key string) Action {
	switch key {
	case "y", "enter":
		id := d.confirmID
		d.workspaceLoading = true
		return ActionCmd{Cmd: d.closeWorkspace(id)}
	case "n", "esc", "q":
		d.confirmID = ""
		d.confirmPath = ""
	}
	return nil
}

func (d *ServerMenu) handleFilterKey(msg tea.KeyPressMsg) Action {
	switch msg.String() {
	case "esc":
		d.filtering = false
		d.filter = ""
		d.browserCursor = 0
	case "enter":
		d.filtering = false
	case "backspace":
		if d.filter != "" {
			_, size := utf8.DecodeLastRuneInString(d.filter)
			d.filter = d.filter[:len(d.filter)-size]
			d.browserCursor = 0
		}
	default:
		if msg.Text != "" && !strings.ContainsAny(msg.Text, "\r\n") {
			d.filter += msg.Text
			d.browserCursor = 0
		}
	}
	return nil
}

func (d *ServerMenu) openProjectForm() {
	d.mode = serverMenuNewProject
	d.returnFromForm = serverMenuBrowser
	d.projectField = projectFieldName
	d.projectError = ""
	d.projectLines = nil
	d.projectName.SetValue("")
	d.projectURL.SetValue("")
	d.projectMode = projectModePlain
	d.projectName.Focus()
	d.projectURL.Blur()
}

func (d *ServerMenu) projectFormFields() []int {
	fields := []int{projectFieldName, projectFieldMode}
	if d.projectMode == projectModeClone {
		fields = append(fields, projectFieldURL)
	}
	return append(fields, projectFieldSubmit)
}

func (d *ServerMenu) setProjectFocus(field int) {
	d.projectField = field
	d.projectName.Blur()
	d.projectURL.Blur()
	switch field {
	case projectFieldName:
		d.projectName.Focus()
	case projectFieldURL:
		d.projectURL.Focus()
	}
}

func (d *ServerMenu) handleProjectKey(msg tea.KeyPressMsg) Action {
	fields := d.projectFormFields()
	index := indexOf(fields, d.projectField)
	switch {
	case key.Matches(msg, d.keyMap.Close):
		d.mode = d.returnFromForm
		d.projectError = ""
		return nil
	case d.projectBusy:
		return nil
	case key.Matches(msg, d.keyMap.Up):
		d.setProjectFocus(fields[(index-1+len(fields))%len(fields)])
		return nil
	case key.Matches(msg, d.keyMap.Down), msg.String() == "tab":
		d.setProjectFocus(fields[(index+1)%len(fields)])
		return nil
	case (msg.String() == "left" || msg.String() == "right") && d.projectField == projectFieldMode:
		d.cycleProjectMode(msg.String() == "right")
		return nil
	case msg.String() == "enter":
		switch d.projectField {
		case projectFieldMode:
			d.cycleProjectMode(true)
			return nil
		case projectFieldSubmit:
			return d.submitProject()
		default:
			d.setProjectFocus(fields[(index+1)%len(fields)])
			return nil
		}
	}
	var cmd tea.Cmd
	switch d.projectField {
	case projectFieldName:
		d.projectName, cmd = d.projectName.Update(msg)
	case projectFieldURL:
		d.projectURL, cmd = d.projectURL.Update(msg)
	}
	return ActionCmd{Cmd: cmd}
}

func (d *ServerMenu) cycleProjectMode(forward bool) {
	modes := []projectCreateMode{projectModePlain, projectModeGitInit, projectModeClone}
	i := indexOf(modes, d.projectMode)
	if forward {
		i = (i + 1) % len(modes)
	} else {
		i = (i - 1 + len(modes)) % len(modes)
	}
	d.projectMode = modes[i]
	if d.projectMode != projectModeClone && d.projectField == projectFieldURL {
		d.setProjectFocus(projectFieldMode)
	}
}

func (d *ServerMenu) submitProject() Action {
	name := strings.TrimSpace(d.projectName.Value())
	if name == "" {
		d.projectError = "Project name is required."
		return nil
	}
	if d.projectMode == projectModeClone && strings.TrimSpace(d.projectURL.Value()) == "" {
		d.projectError = "Clone URL is required."
		return nil
	}
	d.projectError = ""
	d.projectBusy = true
	return ActionCmd{Cmd: d.createProject()}
}

func indexOf[T comparable](values []T, target T) int {
	for i, v := range values {
		if v == target {
			return i
		}
	}
	return 0
}

func (d *ServerMenu) switchRoot(forward bool) Action {
	if d.mode != serverMenuBrowser || len(d.browser.Roots) == 0 || d.browserLoading {
		return nil
	}
	current := 0
	bestLength := -1
	for index, root := range d.browser.Roots {
		if pathWithinRoot(d.browser.Path, root) && len(root) > bestLength {
			current = index
			bestLength = len(root)
		}
	}
	delta := -1
	if forward {
		delta = 1
	}
	next := wrapIndex(current+delta, len(d.browser.Roots))
	d.rememberBrowserSelection()
	d.browserLoading = true
	return ActionCmd{Cmd: d.loadBrowser(d.browser.Roots[next], "")}
}

func (d *ServerMenu) move(delta int) {
	if d.mode == serverMenuWorkspaces {
		d.workspaceCursor = wrapIndex(d.workspaceCursor+delta, len(d.workspaces))
		return
	}
	d.browserCursor = wrapIndex(d.browserCursor+delta, len(d.filteredEntries()))
}

func (d *ServerMenu) selectWorkspace(id string) {
	if id != "" {
		for index := range d.workspaces {
			if d.workspaces[index].ID == id {
				d.workspaceCursor = index
				return
			}
		}
	}
	d.workspaceCursor = clampIndex(d.workspaceCursor, len(d.workspaces))
}

func (d *ServerMenu) selectedWorkspaceID() string {
	if len(d.workspaces) == 0 {
		return ""
	}
	return d.workspaces[d.workspaceCursor].ID
}

func (d *ServerMenu) filteredEntries() []proto.BrowserEntry {
	if d.filter == "" {
		return d.browser.Entries
	}
	needle := strings.ToLower(d.filter)
	entries := make([]proto.BrowserEntry, 0, len(d.browser.Entries))
	for _, entry := range d.browser.Entries {
		if strings.Contains(strings.ToLower(entry.Name), needle) {
			entries = append(entries, entry)
		}
	}
	return entries
}

func (d *ServerMenu) selectedBrowserEntry() (proto.BrowserEntry, bool) {
	entries := d.filteredEntries()
	if len(entries) == 0 {
		return proto.BrowserEntry{}, false
	}
	d.browserCursor = clampIndex(d.browserCursor, len(entries))
	return entries[d.browserCursor], true
}

func (d *ServerMenu) selectedBrowserPath() string {
	entry, ok := d.selectedBrowserEntry()
	if !ok {
		return ""
	}
	return entry.Path
}

func (d *ServerMenu) rememberBrowserSelection() {
	if d.browser.Path == "" {
		return
	}
	if selected := d.selectedBrowserPath(); selected != "" {
		d.browserSelections[d.browser.Path] = selected
	}
}

func (d *ServerMenu) selectBrowserPath(path string) {
	entries := d.filteredEntries()
	if path != "" {
		for index := range entries {
			if entries[index].Path == path {
				d.browserCursor = index
				return
			}
		}
	}
	d.browserCursor = clampIndex(d.browserCursor, len(entries))
}

func (d *ServerMenu) activeRoot() string {
	best := ""
	for _, root := range d.browser.Roots {
		if pathWithinRoot(d.browser.Path, root) && len(root) > len(best) {
			best = root
		}
	}
	return best
}

func pathWithinRoot(path, root string) bool {
	if path == root {
		return true
	}
	if root == "" || !strings.HasPrefix(path, root) || len(path) <= len(root) {
		return false
	}
	separator := path[len(root)]
	return separator == '/' || separator == '\\'
}

func clampIndex(value, length int) int {
	if length == 0 {
		return 0
	}
	if value >= length {
		return length - 1
	}
	return max(0, value)
}

func wrapIndex(value, length int) int {
	if length == 0 {
		return 0
	}
	if value < 0 {
		return length - 1
	}
	return value % length
}

// Draw implements [Dialog].
func (d *ServerMenu) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	if d.mode == serverMenuNewProject {
		return d.drawProjectForm(scr, area)
	}
	return d.drawMenu(scr, area)
}

func (d *ServerMenu) rowStyle(active bool) lipgloss.Style {
	if active {
		return d.styles.Dialog.SelectedItem
	}
	return d.styles.Dialog.NormalItem
}

func (d *ServerMenu) drawMenu(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.styles
	width := max(0, min(84, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := max(0, width-t.Dialog.View.GetHorizontalFrameSize())

	tabs := d.renderTabs()
	rows := []string{tabs, ""}

	listHeight := max(3, min(14, area.Dy()-10))
	if d.mode == serverMenuWorkspaces {
		rows = append(rows, d.renderWorkspaceRows(innerWidth, listHeight)...)
	} else {
		rows = append(rows, d.renderBrowserRows(innerWidth, listHeight)...)
	}

	if status := d.statusLine(); status != "" {
		rows = append(rows, "", t.Dialog.SecondaryText.Render(ansiTruncate(status, innerWidth)))
	}

	rc := NewRenderContext(t, width)
	rc.Title = "Server Workspaces"
	if identity := strings.TrimSpace(strings.Join([]string{d.connectionName, d.connectionAddr}, " ")); identity != "" {
		rc.TitleInfo = " " + t.Dialog.SecondaryText.Render(ansiTruncate(identity, 28))
	}
	rc.AddPart(lipgloss.JoinVertical(lipgloss.Left, rows...))
	rc.Help = renderDialogHelp(t, &d.help, d, innerWidth)
	DrawCenter(scr, area, rc.Render())
	return nil
}

func (d *ServerMenu) renderTabs() string {
	t := d.styles
	workspaces := " Workspaces "
	browser := " Browse & Create "
	if d.mode == serverMenuWorkspaces {
		workspaces = t.Dialog.SelectedItem.Render(workspaces)
		browser = t.Dialog.NormalItem.Render(browser)
	} else {
		workspaces = t.Dialog.NormalItem.Render(workspaces)
		browser = t.Dialog.SelectedItem.Render(browser)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, workspaces, " ", browser)
}

func (d *ServerMenu) renderWorkspaceRows(innerWidth, limit int) []string {
	t := d.styles
	if d.workspaceLoading {
		return []string{t.Dialog.SecondaryText.Render("Loading workspaces…")}
	}
	if d.workspaceError != "" {
		return []string{t.Dialog.TitleError.Render(ansiTruncate("workspaces: "+d.workspaceError, innerWidth))}
	}
	if len(d.workspaces) == 0 {
		return []string{t.Dialog.SecondaryText.Render("No server workspaces. Switch to Browse & Create to open one.")}
	}
	start := visibleWindowStart(d.workspaceCursor, len(d.workspaces), limit)
	rows := make([]string, 0, limit)
	for i := start; i < len(d.workspaces) && i < start+limit; i++ {
		ws := d.workspaces[i]
		state := "idle, closable"
		if ws.ConnectedClients > 0 {
			state = fmt.Sprintf("connected: %d", ws.ConnectedClients)
		}
		line := fmt.Sprintf("%s  [%s]", ansiTruncate(ws.Path, innerWidth-20), state)
		rows = append(rows, d.rowStyle(i == d.workspaceCursor).Render(ansiTruncate(line, innerWidth)))
	}
	if d.confirmID != "" {
		rows = append(rows, "", t.Dialog.TitleError.Render(ansiTruncate("Close idle workspace "+d.confirmPath+"? y/Enter confirm, n/Esc cancel", innerWidth)))
	}
	return rows
}

func (d *ServerMenu) renderBrowserRows(innerWidth, limit int) []string {
	t := d.styles
	rows := make([]string, 0, limit+2)
	if root := d.activeRoot(); root != "" {
		rows = append(rows, t.Dialog.SecondaryText.Render("Root: "+ansiTruncate(root, innerWidth-6)))
	}
	rows = append(rows, t.Dialog.SecondaryText.Render("Path: "+ansiTruncate(d.browser.Path, innerWidth-6)))
	if d.browserLoading {
		rows = append(rows, t.Dialog.SecondaryText.Render("Loading…"))
		return rows
	}
	if d.browserError != "" {
		rows = append(rows, t.Dialog.TitleError.Render(ansiTruncate("browser: "+d.browserError, innerWidth)))
		return rows
	}
	entries := d.filteredEntries()
	if len(entries) == 0 {
		if d.filter != "" {
			rows = append(rows, t.Dialog.SecondaryText.Render("No entries match \u201c"+d.filter+"\u201d."))
		} else {
			rows = append(rows, t.Dialog.SecondaryText.Render("This directory is empty. Press o to open it, or n for a new project."))
		}
		return rows
	}
	start := visibleWindowStart(d.browserCursor, len(entries), limit)
	for i := start; i < len(entries) && i < start+limit; i++ {
		entry := entries[i]
		kind := "f "
		name := entry.Name
		if entry.Directory {
			kind = "d "
			name += "/"
		}
		rows = append(rows, d.rowStyle(i == d.browserCursor).Render(ansiTruncate(kind+name, innerWidth)))
	}
	if d.browser.Truncated {
		rows = append(rows, t.Dialog.SecondaryText.Render("Listing truncated by the server."))
	}
	if d.filtering {
		rows = append(rows, t.Dialog.SecondaryText.Render("Filter: "+d.filter+"_  Enter apply  Esc clear"))
	}
	return rows
}

func (d *ServerMenu) statusLine() string {
	if d.statusText != "" {
		return d.statusText
	}
	if d.mode == serverMenuWorkspaces {
		return "Enter reattach  d close idle  r refresh  Tab browse/create  Esc quit"
	}
	return "Enter open dir  Backspace parent  ←/→ roots  / filter  o open here  n new project  Tab workspaces"
}

func (d *ServerMenu) drawProjectForm(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.styles
	width := max(0, min(70, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	labelWidth := 14
	inputWidth := max(1, innerWidth-labelWidth)

	fields := d.projectFormFields()
	rows := make([]string, 0, len(fields)+2)
	cursorRow := -1
	for row, field := range fields {
		switch field {
		case projectFieldName:
			d.projectName.SetWidth(inputWidth)
			rows = append(rows, d.projectInputLine("Name", d.projectName.View(), field))
			if field == d.projectField {
				cursorRow = row
			}
		case projectFieldURL:
			d.projectURL.SetWidth(inputWidth)
			rows = append(rows, d.projectInputLine("Clone URL", d.projectURL.View(), field))
			if field == d.projectField {
				cursorRow = row
			}
		case projectFieldMode:
			rows = append(rows, d.rowStyle(field == d.projectField).Render(" Mode: "+d.projectMode.label()+"  (←/→)"))
		case projectFieldSubmit:
			label := "Create project"
			if d.projectBusy {
				label = "Creating…"
			}
			rows = append(rows, d.rowStyle(field == d.projectField).Render(" "+label))
		}
	}
	if len(d.projectLines) > 0 {
		last := d.projectLines[len(d.projectLines)-1]
		rows = append(rows, "", t.Dialog.SecondaryText.Render(ansiTruncate(last, innerWidth)))
	}
	if d.projectError != "" {
		rows = append(rows, "", t.Dialog.TitleError.Render(ansiTruncate(d.projectError, innerWidth)))
	}

	rc := NewRenderContext(t, width)
	rc.Title = "New Project"
	rc.TitleInfo = " " + t.Dialog.SecondaryText.Render("under "+ansiTruncate(d.activeRoot(), 24))
	rc.AddPart(lipgloss.JoinVertical(lipgloss.Left, rows...))
	rc.Help = renderDialogHelp(t, &d.help, d, innerWidth)
	view := rc.Render()
	if cursorRow < 0 || d.projectBusy {
		DrawCenter(scr, area, view)
		return nil
	}
	var cur *tea.Cursor
	switch d.projectField {
	case projectFieldName:
		cur = InputCursor(t, d.projectName.Cursor())
	case projectFieldURL:
		cur = InputCursor(t, d.projectURL.Cursor())
	}
	if cur != nil {
		cur.X += labelWidth
		cur.Y += cursorRow
	}
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

func (d *ServerMenu) projectInputLine(label, value string, field int) string {
	style := d.styles.Dialog.Arguments.InputLabelBlurred
	if field == d.projectField {
		style = d.styles.Dialog.Arguments.InputLabelFocused
	}
	return style.Width(14).Render(label+":") + value
}

// ShortHelp implements [help.KeyMap].
func (d *ServerMenu) ShortHelp() []key.Binding {
	if d.mode == serverMenuNewProject {
		return []key.Binding{d.keyMap.Up, d.keyMap.Down, d.keyMap.Enter, d.keyMap.Close}
	}
	return []key.Binding{d.keyMap.Up, d.keyMap.Down, d.keyMap.Enter, d.keyMap.Tab, d.keyMap.New, d.keyMap.Refresh, d.keyMap.Close}
}

// FullHelp implements [help.KeyMap].
func (d *ServerMenu) FullHelp() [][]key.Binding {
	return [][]key.Binding{d.ShortHelp()}
}

func ansiTruncate(value string, width int) string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\r", " ")
	return ansi.Truncate(value, max(0, width), "…")
}

func visibleWindowStart(selected, length, limit int) int {
	if length <= limit || selected < limit {
		return 0
	}
	start := selected - limit + 1
	if start+limit > length {
		start = length - limit
	}
	return start
}
