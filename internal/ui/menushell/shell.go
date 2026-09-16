// Package menushell hosts the server workspace menu ([dialog.ServerMenu]) as
// a real dialog.Dialog inside a minimal dialog.Overlay stack, for the window
// between "connected to a remote server" and "attached to a workspace" where
// no common.Common (and therefore no full ui.UI) exists yet. Once the menu
// produces a selection, the caller (internal/cmd) tears this shell down and
// proceeds through the existing workspace-creation/attach flow into the real
// ui.New-based TUI.
package menushell

import (
	"context"
	"strings"

	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/styles"
)

// Model is a small tea.Model that owns a *dialog.Overlay hosting exactly one
// dialog.ServerMenu. It exits (Quitting() == true) either when the menu
// dialog reports a final selection, or when the menu is closed/dismissed
// (matching the previous standalone servermenu.Model's "no selection means
// return nil" contract).
type Model struct {
	ctx    context.Context
	client dialog.ServerMenuClient

	connectionName string
	connectionAddr string
	initialError   error

	styles  styles.Styles
	width   int
	height  int
	overlay *dialog.Overlay
	menu    *dialog.ServerMenu

	selection dialog.ServerMenuSelection
	quitting  bool
}

// New builds a menu shell for the given authenticated remote client. Call
// SetConnection/SetError before Init runs (i.e. before handing the model to
// a tea.Program) to have them reflected in the very first frame.
func New(ctx context.Context, client dialog.ServerMenuClient) *Model {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Model{
		ctx:    ctx,
		client: client,
		styles: styles.ThemeForProvider(""),
		width:  100,
		height: 30,
	}
}

// SetConnection records the saved connection's display name/address so the
// menu dialog can show which server it is talking to.
func (m *Model) SetConnection(name, address string) {
	m.connectionName = name
	m.connectionAddr = address
}

// SetError records an error from a previous iteration (e.g. a failed attempt
// to open a previously selected workspace) to surface once the menu opens.
func (m *Model) SetError(err error) {
	m.initialError = err
}

// Selection returns the terminal selection once Quitting() is true. A zero
// value means the user closed the menu without picking anything.
func (m *Model) Selection() dialog.ServerMenuSelection {
	return m.selection
}

// Quitting reports whether the shell has finished (selection made, or the
// menu was dismissed).
func (m *Model) Quitting() bool {
	return m.quitting
}

// Init implements [tea.Model].
func (m *Model) Init() tea.Cmd {
	menu, cmd := dialog.NewServerMenu(m.ctx, m.client, m.connectionName, m.connectionAddr)
	if m.initialError != nil {
		menu.SetStatusError(m.initialError)
	}
	m.menu = menu
	m.overlay = dialog.NewOverlay(menu)
	return cmd
}

// Update implements [tea.Model].
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	}
	if m.overlay == nil || !m.overlay.HasDialogs() {
		return m, nil
	}
	return m.handleAction(m.overlay.Update(msg))
}

func (m *Model) handleAction(action dialog.Action) (tea.Model, tea.Cmd) {
	switch a := action.(type) {
	case dialog.ActionServerMenuSelected:
		m.selection = a.Selection
		m.quitting = true
		return m, tea.Quit
	case dialog.ActionClose:
		// The menu is the only dialog in this shell and is not meant to be
		// dismissable independently of the shell itself: closing it (esc,
		// ctrl+c) means "give up on the menu", matching the previous
		// standalone servermenu.Model's q/esc behavior.
		m.quitting = true
		return m, tea.Quit
	case dialog.ActionCmd:
		return m, a.Cmd
	default:
		return m, nil
	}
}

// View implements [tea.Model].
func (m *Model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.BackgroundColor = m.styles.Background
	v.WindowTitle = "crux server workspaces"
	v.MouseMode = tea.MouseModeCellMotion

	canvas := uv.NewScreenBuffer(m.width, m.height)
	if m.overlay != nil {
		v.Cursor = m.overlay.Draw(canvas, canvas.Bounds())
	}

	content := strings.ReplaceAll(canvas.Render(), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	v.Content = strings.Join(lines, "\n")
	return v
}
