package dialog

import (
	"context"
	"errors"
	"testing"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

// fakeServerMenuClient is a scriptable in-memory implementation of
// [ServerMenuClient] used to drive the [ServerMenu] dialog without any real
// transport.
type fakeServerMenuClient struct {
	workspaces []proto.Workspace
	listing    proto.BrowserListing
	listings   map[string]proto.BrowserListing

	listCalls  int
	browseCall []string
	closed     []string

	workspaceErr error
	browserErr   error
	closeErr     error

	createErr   error
	createPath  string
	createLines []string
	createReq   proto.PeerWorkspaceCreateRequest
}

func (f *fakeServerMenuClient) RefreshWorkspaces(context.Context) ([]proto.Workspace, error) {
	f.listCalls++
	return f.workspaces, f.workspaceErr
}

func (f *fakeServerMenuClient) Browse(_ context.Context, path string) (proto.BrowserListing, error) {
	f.browseCall = append(f.browseCall, path)
	if f.browserErr != nil {
		return proto.BrowserListing{}, f.browserErr
	}
	if listing, ok := f.listings[path]; ok {
		return listing, nil
	}
	return f.listing, nil
}

func (f *fakeServerMenuClient) CloseIdleWorkspace(_ context.Context, id string) error {
	f.closed = append(f.closed, id)
	return f.closeErr
}

func (f *fakeServerMenuClient) CreateProject(_ context.Context, request proto.PeerWorkspaceCreateRequest, progress func(string)) (string, error) {
	f.createReq = request
	for _, line := range f.createLines {
		progress(line)
	}
	if f.createErr != nil {
		return "", f.createErr
	}
	return f.createPath, nil
}

// smKey builds a synthetic key press, mirroring [tea.Key]'s Code/Text pair.
// Named to avoid colliding with the single-rune keyMsg helper already
// declared for other dialog tests in this package.
func smKey(code rune, text string) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Text: text})
}

// settleServerMenu delivers msg to d, then recursively resolves any
// [ActionCmd] (and any [tea.BatchMsg]) the dialog returns so the dialog's
// state fully settles before the test inspects it, mirroring how the real
// Overlay/tea runtime would drive follow-up commands to completion.
func settleServerMenu(t *testing.T, d *ServerMenu, msg tea.Msg) Action {
	t.Helper()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var last Action
		for _, cmd := range batch {
			if cmd == nil {
				continue
			}
			last = settleServerMenu(t, d, cmd())
		}
		return last
	}
	action := d.HandleMsg(msg)
	if ac, ok := action.(ActionCmd); ok && ac.Cmd != nil {
		return settleServerMenu(t, d, ac.Cmd())
	}
	return action
}

func initServerMenu(t *testing.T, d *ServerMenu, cmd tea.Cmd) {
	t.Helper()
	require.NotNil(t, cmd)
	settleServerMenu(t, d, cmd())
}

func drawServerMenu(d *ServerMenu, width, height int) string {
	screen := uv.NewScreenBuffer(width, height)
	d.Draw(screen, screen.Bounds())
	return ansi.Strip(screen.Render())
}

func TestServerMenuInitialLoadIsAsynchronous(t *testing.T) {
	client := &fakeServerMenuClient{listing: proto.BrowserListing{Path: "/srv"}}
	d, cmd := NewServerMenu(t.Context(), client, "office", "tcp://server.example:9090")
	require.Zero(t, client.listCalls)
	require.Empty(t, client.browseCall)
	initServerMenu(t, d, cmd)
	require.Equal(t, 1, client.listCalls)
	require.Equal(t, []string{""}, client.browseCall)
}

func TestServerMenuWorkspaceSelection(t *testing.T) {
	client := &fakeServerMenuClient{
		workspaces: []proto.Workspace{{ID: "one", Path: "/one"}, {ID: "two", Path: "/two"}},
		listing:    proto.BrowserListing{Path: "/srv"},
	}
	d, cmd := NewServerMenu(t.Context(), client, "", "")
	initServerMenu(t, d, cmd)

	settleServerMenu(t, d, smKey('j', "j"))
	action := settleServerMenu(t, d, smKey(tea.KeyEnter, ""))
	sel, ok := action.(ActionServerMenuSelected)
	require.True(t, ok)
	require.Equal(t, "two", sel.Selection.WorkspaceID)
}

func TestServerMenuBrowserNavigationAndOpen(t *testing.T) {
	client := &fakeServerMenuClient{
		listing: proto.BrowserListing{Path: "/srv", Parent: "/", Entries: []proto.BrowserEntry{{Name: "child", Path: "/srv/child", Directory: true}}},
		listings: map[string]proto.BrowserListing{
			"/srv/child": {Path: "/srv/child", Parent: "/srv"},
		},
	}
	d, cmd := NewServerMenu(t.Context(), client, "", "")
	initServerMenu(t, d, cmd)

	settleServerMenu(t, d, smKey(tea.KeyTab, ""))
	settleServerMenu(t, d, smKey(tea.KeyEnter, ""))
	require.Equal(t, "/srv/child", client.browseCall[len(client.browseCall)-1])

	action := settleServerMenu(t, d, smKey('o', "o"))
	sel, ok := action.(ActionServerMenuSelected)
	require.True(t, ok)
	require.Equal(t, "/srv/child", sel.Selection.Path)
}

func TestServerMenuRootSwitchingFilteringAndSelectionPreservation(t *testing.T) {
	rootListing := proto.BrowserListing{
		Roots:   []string{"/srv", "/opt"},
		Path:    "/srv",
		Entries: []proto.BrowserEntry{{Name: "alpha", Path: "/srv/alpha", Directory: true}, {Name: "beta.txt", Path: "/srv/beta.txt"}, {Name: "gamma", Path: "/srv/gamma", Directory: true}},
	}
	client := &fakeServerMenuClient{
		listing: rootListing,
		listings: map[string]proto.BrowserListing{
			"/srv":       rootListing,
			"/srv/gamma": {Roots: rootListing.Roots, Path: "/srv/gamma", Parent: "/srv"},
			"/opt":       {Roots: rootListing.Roots, Path: "/opt", Entries: []proto.BrowserEntry{{Name: "project", Path: "/opt/project", Directory: true}}},
		},
	}
	d, cmd := NewServerMenu(t.Context(), client, "", "")
	initServerMenu(t, d, cmd)

	settleServerMenu(t, d, smKey(tea.KeyTab, ""))
	settleServerMenu(t, d, smKey('/', "/"))
	for _, character := range "alph" {
		settleServerMenu(t, d, smKey(character, string(character)))
	}
	require.Equal(t, "alph", d.filter)
	require.Len(t, d.filteredEntries(), 1)
	require.Contains(t, drawServerMenu(d, 100, 30), "Filter: alph")
	settleServerMenu(t, d, smKey(tea.KeyEsc, ""))
	require.Empty(t, d.filter)

	settleServerMenu(t, d, smKey('j', "j"))
	settleServerMenu(t, d, smKey('j', "j"))
	require.Equal(t, "/srv/gamma", d.selectedBrowserPath())

	settleServerMenu(t, d, smKey('r', "r"))
	require.Equal(t, "/srv/gamma", d.selectedBrowserPath())

	settleServerMenu(t, d, smKey(tea.KeyEnter, ""))
	require.Equal(t, "/srv/gamma", d.browser.Path)

	settleServerMenu(t, d, smKey(tea.KeyBackspace, ""))
	require.Equal(t, "/srv/gamma", d.selectedBrowserPath())

	settleServerMenu(t, d, smKey(tea.KeyRight, ""))
	require.Equal(t, "/opt", d.browser.Path)
	require.Contains(t, drawServerMenu(d, 100, 30), "Root: /opt")
}

func TestServerMenuRefreshTargetsActivePaneAndKeepsIndependentErrors(t *testing.T) {
	client := &fakeServerMenuClient{workspaces: []proto.Workspace{{ID: "idle", Path: "/idle"}}, listing: proto.BrowserListing{Path: "/srv"}}
	d, cmd := NewServerMenu(t.Context(), client, "", "")
	initServerMenu(t, d, cmd)
	require.Equal(t, 1, client.listCalls)
	require.Len(t, client.browseCall, 1)

	settleServerMenu(t, d, smKey('r', "r"))
	require.Equal(t, 2, client.listCalls)
	require.Len(t, client.browseCall, 1)

	settleServerMenu(t, d, smKey(tea.KeyTab, ""))
	client.browserErr = errors.New("browser unavailable")
	settleServerMenu(t, d, smKey('r', "r"))
	require.Equal(t, 2, client.listCalls)
	require.Len(t, client.browseCall, 2)
	require.Len(t, d.workspaces, 1)
	require.Contains(t, drawServerMenu(d, 100, 30), "browser: browser unavailable")

	client.browserErr = nil
	settleServerMenu(t, d, smKey('r', "r"))
	require.Empty(t, d.browserError)
}

func TestServerMenuCloseIdleConfirmationAndErrors(t *testing.T) {
	client := &fakeServerMenuClient{
		workspaces: []proto.Workspace{{ID: "connected", Path: "/connected", ConnectedClients: 1}, {ID: "idle", Path: "/idle"}},
		listing:    proto.BrowserListing{Path: "/srv"},
	}
	d, cmd := NewServerMenu(t.Context(), client, "", "")
	initServerMenu(t, d, cmd)

	settleServerMenu(t, d, smKey('d', "d"))
	require.Empty(t, d.confirmID)
	require.Contains(t, d.statusText, "cannot be closed")

	settleServerMenu(t, d, smKey('j', "j"))
	settleServerMenu(t, d, smKey('d', "d"))
	require.Equal(t, "idle", d.confirmID)
	require.Contains(t, drawServerMenu(d, 100, 30), "/idle")

	client.closeErr = errors.New("close failed")
	settleServerMenu(t, d, smKey('y', "y"))
	require.Equal(t, []string{"idle"}, client.closed)
	require.Contains(t, drawServerMenu(d, 100, 30), "close failed")
}

func TestServerMenuConnectionIdentityAndEmptyDirectory(t *testing.T) {
	client := &fakeServerMenuClient{listing: proto.BrowserListing{Path: "/srv", Entries: []proto.BrowserEntry{{Name: "file.txt", Path: "/srv/file.txt"}}}}
	d, cmd := NewServerMenu(t.Context(), client, "office", "tcp://server.example:9090")
	initServerMenu(t, d, cmd)

	settleServerMenu(t, d, smKey(tea.KeyTab, ""))
	settleServerMenu(t, d, smKey(tea.KeyEnter, ""))
	require.Contains(t, d.statusText, "Select a directory")

	// At a comfortable width the connection identity sits beside the title.
	view := drawServerMenu(d, 100, 24)
	require.Contains(t, view, "office")
	require.Contains(t, view, "Workspaces")
	require.Contains(t, view, "Browse")

	// At a narrow width, RenderContext.Render intentionally drops title info
	// entirely (rather than truncating a styled fragment) when it can't sit
	// beside the title with at least a one-cell gap; the dialog must still
	// render the rest of its content without panicking.
	narrow := drawServerMenu(d, 48, 20)
	require.Contains(t, narrow, "Workspaces")
	require.Contains(t, narrow, "Browse")
}

func TestServerMenuNewProjectFormValidationAndSubmit(t *testing.T) {
	client := &fakeServerMenuClient{listing: proto.BrowserListing{Roots: []string{"/srv"}, Path: "/srv"}, createPath: "/srv/demo"}
	d, cmd := NewServerMenu(t.Context(), client, "", "")
	initServerMenu(t, d, cmd)

	settleServerMenu(t, d, smKey(tea.KeyTab, ""))
	settleServerMenu(t, d, smKey('n', "n"))
	require.Equal(t, serverMenuNewProject, d.mode)
	require.Equal(t, projectFieldName, d.projectField)

	// Submitting with an empty name is rejected before any client call.
	settleServerMenu(t, d, smKey('j', "j")) // Name -> Mode
	settleServerMenu(t, d, smKey('j', "j")) // Mode -> Submit
	action := settleServerMenu(t, d, smKey(tea.KeyEnter, ""))
	require.Nil(t, action)
	require.Contains(t, d.projectError, "name is required")
	require.Zero(t, client.createReq.RelativePath)

	// Back to Name, fill it in, then switch to clone mode and fill the URL.
	settleServerMenu(t, d, smKey('k', "k")) // Submit -> Mode
	settleServerMenu(t, d, smKey('k', "k")) // Mode -> Name
	// Typed characters are dispatched directly (not through
	// settleServerMenu) because the underlying textinput's virtual-cursor
	// blink resets on every keystroke and returns a real timer-based
	// tea.Cmd; running that cmd synchronously (as settleServerMenu does for
	// ActionCmd) would make typing tests block for real wall-clock time for
	// no assertion benefit.
	for _, character := range "demo" {
		d.HandleMsg(smKey(character, string(character)))
	}
	require.Equal(t, "demo", d.projectName.Value())

	settleServerMenu(t, d, smKey('j', "j")) // Name -> Mode
	settleServerMenu(t, d, smKey(tea.KeyRight, ""))
	settleServerMenu(t, d, smKey(tea.KeyRight, ""))
	require.Equal(t, projectModeClone, d.projectMode)

	settleServerMenu(t, d, smKey('j', "j")) // Mode -> URL
	require.Equal(t, projectFieldURL, d.projectField)
	const cloneURL = "https://example.com/user/repo.git"
	for _, character := range cloneURL {
		d.HandleMsg(smKey(character, string(character)))
	}
	require.Equal(t, cloneURL, d.projectURL.Value())

	settleServerMenu(t, d, smKey('j', "j")) // URL -> Submit
	require.Equal(t, projectFieldSubmit, d.projectField)
	action = settleServerMenu(t, d, smKey(tea.KeyEnter, ""))

	sel, ok := action.(ActionServerMenuSelected)
	require.True(t, ok)
	require.Equal(t, "/srv/demo", sel.Selection.Path)
	require.Equal(t, "demo", client.createReq.RelativePath)
	require.Equal(t, "/srv", client.createReq.Root)
	require.Equal(t, proto.PeerWorkspaceCreateClone, client.createReq.Mode)
	require.Equal(t, cloneURL, client.createReq.CloneURL)
}

func TestServerMenuNewProjectFormEscapeReturnsToBrowser(t *testing.T) {
	client := &fakeServerMenuClient{listing: proto.BrowserListing{Path: "/srv"}}
	d, cmd := NewServerMenu(t.Context(), client, "", "")
	initServerMenu(t, d, cmd)

	settleServerMenu(t, d, smKey(tea.KeyTab, ""))
	settleServerMenu(t, d, smKey('n', "n"))
	require.Equal(t, serverMenuNewProject, d.mode)

	action := settleServerMenu(t, d, smKey(tea.KeyEsc, ""))
	require.Nil(t, action)
	require.Equal(t, serverMenuBrowser, d.mode)
}

func TestServerMenuDrawProjectFormRendersFields(t *testing.T) {
	client := &fakeServerMenuClient{listing: proto.BrowserListing{Path: "/srv"}}
	d, cmd := NewServerMenu(t.Context(), client, "", "")
	initServerMenu(t, d, cmd)
	settleServerMenu(t, d, smKey(tea.KeyTab, ""))
	settleServerMenu(t, d, smKey('n', "n"))

	view := drawServerMenu(d, 90, 24)
	require.Contains(t, view, "New Project")
	require.Contains(t, view, "Name")

	// The project inputs use the virtual-cursor text-input mode (matching
	// this codebase's established convention, e.g. MCPServers' form
	// inputs): the cursor glyph is baked into the rendered text itself, so
	// Draw legitimately returns a nil *tea.Cursor here. What matters is
	// that drawing with an active, focused input does not panic.
	screen := uv.NewScreenBuffer(90, 24)
	require.NotPanics(t, func() { d.Draw(screen, screen.Bounds()) })
}
