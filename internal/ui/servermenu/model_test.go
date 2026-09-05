package servermenu

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

type fakeClient struct {
	workspaces   []proto.Workspace
	listing      proto.BrowserListing
	listings     map[string]proto.BrowserListing
	listCalls    int
	browse       []string
	closed       []string
	workspaceErr error
	browserErr   error
	closeErr     error
}

func (f *fakeClient) RefreshWorkspaces(context.Context) ([]proto.Workspace, error) {
	f.listCalls++
	return f.workspaces, f.workspaceErr
}

func (f *fakeClient) Browse(_ context.Context, path string) (proto.BrowserListing, error) {
	f.browse = append(f.browse, path)
	if f.browserErr != nil {
		return proto.BrowserListing{}, f.browserErr
	}
	if listing, ok := f.listings[path]; ok {
		return listing, nil
	}
	return f.listing, nil
}

func (f *fakeClient) CloseIdleWorkspace(_ context.Context, id string) error {
	f.closed = append(f.closed, id)
	return f.closeErr
}

func key(code rune, text string) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Text: text})
}

func initialize(t *testing.T, model *Model) {
	t.Helper()
	runCommand(t, model, model.Init())
}

func runCommand(t *testing.T, model *Model, command tea.Cmd) {
	t.Helper()
	require.NotNil(t, command)
	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		for _, child := range batch {
			runCommand(t, model, child)
		}
		return
	}
	model.Update(message)
}

func TestInitializationIsAsynchronous(t *testing.T) {
	client := &fakeClient{listing: proto.BrowserListing{Path: "/srv"}}
	model := New(t.Context(), client)
	command := model.Init()
	require.Zero(t, client.listCalls)
	require.Empty(t, client.browse)
	runCommand(t, model, command)
	require.Equal(t, 1, client.listCalls)
	require.Equal(t, []string{""}, client.browse)
}

func TestWorkspaceAndBrowserSelections(t *testing.T) {
	client := &fakeClient{
		workspaces: []proto.Workspace{{ID: "one", Path: "/one"}, {ID: "two", Path: "/two"}},
		listing:    proto.BrowserListing{Path: "/srv", Parent: "/", Entries: []proto.BrowserEntry{{Name: "child", Path: "/srv/child", Directory: true}}},
	}
	model := New(t.Context(), client)
	initialize(t, model)
	model.Update(key('j', "j"))
	_, command := model.Update(key(tea.KeyEnter, ""))
	require.NotNil(t, command)
	require.Equal(t, "two", model.Selection().WorkspaceID)

	model = New(t.Context(), client)
	initialize(t, model)
	model.Update(key(tea.KeyTab, ""))
	_, command = model.Update(key(tea.KeyEnter, ""))
	runCommand(t, model, command)
	require.Equal(t, "/srv/child", client.browse[len(client.browse)-1])
	model.Update(key('o', "o"))
	require.Equal(t, "/srv", model.Selection().Path)
}

func TestBrowserRootSwitchingFilteringAndSelectionPreservation(t *testing.T) {
	rootListing := proto.BrowserListing{
		Roots:   []string{"/srv", "/opt"},
		Path:    "/srv",
		Entries: []proto.BrowserEntry{{Name: "alpha", Path: "/srv/alpha", Directory: true}, {Name: "beta.txt", Path: "/srv/beta.txt"}, {Name: "gamma", Path: "/srv/gamma", Directory: true}},
	}
	client := &fakeClient{
		listing: rootListing,
		listings: map[string]proto.BrowserListing{
			"/srv":       rootListing,
			"/srv/gamma": {Roots: rootListing.Roots, Path: "/srv/gamma", Parent: "/srv"},
			"/opt":       {Roots: rootListing.Roots, Path: "/opt", Entries: []proto.BrowserEntry{{Name: "project", Path: "/opt/project", Directory: true}}},
		},
	}
	model := New(t.Context(), client)
	initialize(t, model)
	model.Update(key(tea.KeyTab, ""))
	model.Update(key('/', "/"))
	for _, character := range "alph" {
		model.Update(key(character, string(character)))
	}
	require.Equal(t, "alph", model.filter)
	require.Len(t, model.filteredEntries(), 1)
	require.Contains(t, model.View().Content, "Filter: alph")
	model.Update(key(tea.KeyEsc, ""))
	require.Empty(t, model.filter)

	model.Update(key('j', "j"))
	model.Update(key('j', "j"))
	require.Equal(t, "/srv/gamma", model.selectedBrowserPath())
	_, command := model.Update(key('r', "r"))
	runCommand(t, model, command)
	require.Equal(t, "/srv/gamma", model.selectedBrowserPath())
	_, command = model.Update(key(tea.KeyEnter, ""))
	runCommand(t, model, command)
	_, command = model.Update(key(tea.KeyLeft, ""))
	runCommand(t, model, command)
	require.Equal(t, "/srv/gamma", model.selectedBrowserPath())

	_, command = model.Update(key(']', "]"))
	runCommand(t, model, command)
	require.Equal(t, "/opt", model.browser.Path)
	require.Contains(t, model.View().Content, "Root: /opt")
}

func TestRefreshTargetsActivePaneAndKeepsIndependentErrors(t *testing.T) {
	client := &fakeClient{workspaces: []proto.Workspace{{ID: "idle", Path: "/idle"}}, listing: proto.BrowserListing{Path: "/srv"}}
	model := New(t.Context(), client)
	initialize(t, model)
	require.Equal(t, 1, client.listCalls)
	require.Len(t, client.browse, 1)

	_, command := model.Update(key('r', "r"))
	runCommand(t, model, command)
	require.Equal(t, 2, client.listCalls)
	require.Len(t, client.browse, 1)
	model.Update(key(tea.KeyTab, ""))
	client.browserErr = errors.New("browser unavailable")
	_, command = model.Update(key('r', "r"))
	runCommand(t, model, command)
	require.Equal(t, 2, client.listCalls)
	require.Len(t, client.browse, 2)
	require.Len(t, model.workspaces, 1)
	require.Contains(t, model.View().Content, "browser: browser unavailable")

	client.browserErr = nil
	_, command = model.Update(key('r', "r"))
	runCommand(t, model, command)
	require.Empty(t, model.browserError)
}

func TestCloseAvailabilityConfirmationAndErrors(t *testing.T) {
	client := &fakeClient{
		workspaces: []proto.Workspace{{ID: "connected", Path: "/connected", ConnectedClients: 1}, {ID: "idle", Path: "/idle"}},
		listing:    proto.BrowserListing{Path: "/srv"},
	}
	model := New(t.Context(), client)
	initialize(t, model)
	model.Update(key('d', "d"))
	require.Empty(t, model.confirmID)
	require.Contains(t, model.statusText, "cannot be closed")
	model.Update(key('j', "j"))
	model.Update(key('d', "d"))
	require.Equal(t, "idle", model.confirmID)
	require.Contains(t, model.View().Content, "/idle")
	client.closeErr = errors.New("close failed")
	_, command := model.Update(key('y', "y"))
	runCommand(t, model, command)
	require.Equal(t, []string{"idle"}, client.closed)
	require.Contains(t, model.View().Content, "close failed")
}

func TestConnectionIdentityUnavailableActionsAndNarrowLayout(t *testing.T) {
	client := &fakeClient{listing: proto.BrowserListing{Path: "/srv", Entries: []proto.BrowserEntry{{Name: "file.txt", Path: "/srv/file.txt"}}}}
	model := New(t.Context(), client)
	model.SetConnection("office", "tcp://server.example:9090")
	initialize(t, model)
	model.Update(tea.WindowSizeMsg{Width: 48, Height: 20})
	model.Update(key(tea.KeyTab, ""))
	model.Update(key(tea.KeyEnter, ""))
	require.Contains(t, model.statusText, "Select a directory")
	view := model.View().Content
	require.Contains(t, view, "office")
	require.Contains(t, view, "Workspaces")
	require.Contains(t, view, "Server files")
}
