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
	workspaces []proto.Workspace
	listing    proto.BrowserListing
	listCalls  int
	browse     []string
	closed     []string
	err        error
}

func (f *fakeClient) RefreshWorkspaces(context.Context) ([]proto.Workspace, error) {
	f.listCalls++
	return f.workspaces, f.err
}

func (f *fakeClient) Browse(_ context.Context, path string) (proto.BrowserListing, error) {
	f.browse = append(f.browse, path)
	return f.listing, f.err
}

func (f *fakeClient) CloseIdleWorkspace(_ context.Context, id string) error {
	f.closed = append(f.closed, id)
	return f.err
}

func key(code rune, text string) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Text: text})
}

func initialize(t *testing.T, model *Model) {
	t.Helper()
	batch, ok := model.Init()().(tea.BatchMsg)
	require.True(t, ok)
	for _, command := range batch {
		model.Update(command())
	}
}

func TestInitializationIsAsynchronous(t *testing.T) {
	client := &fakeClient{listing: proto.BrowserListing{Path: "/srv"}}
	model := New(t.Context(), client)
	command := model.Init()
	require.Zero(t, client.listCalls)
	require.Empty(t, client.browse)
	require.NotNil(t, command)
	batch := command().(tea.BatchMsg)
	for _, child := range batch {
		model.Update(child())
	}
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
	require.NotNil(t, command)
	model.Update(command())
	require.Equal(t, "/srv/child", client.browse[len(client.browse)-1])
	model.Update(key('o', "o"))
	require.Equal(t, "/srv", model.Selection().Path)
}

func TestRefreshErrorsCloseConfirmationAndQuit(t *testing.T) {
	client := &fakeClient{workspaces: []proto.Workspace{{ID: "idle", Path: "/idle"}}, listing: proto.BrowserListing{Path: "/srv"}}
	model := New(t.Context(), client)
	initialize(t, model)
	model.Update(key('d', "d"))
	require.Equal(t, "idle", model.confirmID)
	_, command := model.Update(key('y', "y"))
	require.NotNil(t, command)
	model.Update(command())
	require.Equal(t, []string{"idle"}, client.closed)

	client.err = errors.New("connection lost")
	_, command = model.Update(key('r', "r"))
	batch := command().(tea.BatchMsg)
	for _, child := range batch {
		model.Update(child())
	}
	require.Contains(t, model.View().Content, "connection lost")
	_, command = model.Update(key('q', "q"))
	require.NotNil(t, command)
	require.True(t, model.Quitting())
}
