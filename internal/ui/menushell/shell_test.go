package menushell

import (
	"context"
	"errors"
	"testing"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// fakeMenuClient is a minimal in-memory [dialog.ServerMenuClient] used to
// drive the shell without any real transport.
type fakeMenuClient struct {
	workspaces []proto.Workspace
	listing    proto.BrowserListing
}

func (f *fakeMenuClient) RefreshWorkspaces(context.Context) ([]proto.Workspace, error) {
	return f.workspaces, nil
}

func (f *fakeMenuClient) Browse(context.Context, string) (proto.BrowserListing, error) {
	return f.listing, nil
}

func (f *fakeMenuClient) CloseIdleWorkspace(context.Context, string) error { return nil }

func (f *fakeMenuClient) CreateProject(context.Context, proto.PeerWorkspaceCreateRequest, func(string)) (string, error) {
	return "", errors.New("not implemented")
}

// settle runs a tea.Cmd (and recursively any tea.BatchMsg/follow-up
// ActionCmd it produces) through the model's Update, mirroring how a real
// tea.Program would drive commands to completion, so tests can synchronously
// observe the settled state.
func settle(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	deliver(t, m, cmd())
}

func deliver(t *testing.T, m *Model, msg tea.Msg) {
	t.Helper()
	if msg == nil {
		return
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			settle(t, m, c)
		}
		return
	}
	_, cmd := m.Update(msg)
	settle(t, m, cmd)
}

func TestNewAppliesDefaults(t *testing.T) {
	m := New(t.Context(), &fakeMenuClient{})
	require.False(t, m.Quitting())
	require.Equal(t, dialog.ServerMenuSelection{}, m.Selection())
}

func TestInitBuildsOverlayAndAppliesInitialError(t *testing.T) {
	m := New(t.Context(), &fakeMenuClient{listing: proto.BrowserListing{Path: "/srv"}})
	m.SetConnection("office", "tcp://server.example:9090")
	m.SetError(errors.New("previous attempt failed"))

	cmd := m.Init()
	require.NotNil(t, m.overlay)
	require.True(t, m.overlay.HasDialogs())

	settle(t, m, cmd)
	view := m.View()
	require.Contains(t, view.Content, "previous attempt failed")
	require.Contains(t, view.Content, "office")
}

func TestUpdateWindowSizeMsgResizesCanvas(t *testing.T) {
	m := New(t.Context(), &fakeMenuClient{})
	settle(t, m, m.Init())
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	require.Nil(t, cmd)
	require.Equal(t, 120, m.width)
	require.Equal(t, 40, m.height)
}

func TestSelectingAWorkspaceQuitsWithSelection(t *testing.T) {
	client := &fakeMenuClient{workspaces: []proto.Workspace{{ID: "ws-1", Path: "/ws-1"}}}
	m := New(t.Context(), client)
	settle(t, m, m.Init())

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, m.Quitting())
	require.Equal(t, "ws-1", m.Selection().WorkspaceID)
	require.NotNil(t, cmd)
	require.IsType(t, tea.QuitMsg{}, cmd())
}

func TestClosingTheMenuQuitsWithoutSelection(t *testing.T) {
	m := New(t.Context(), &fakeMenuClient{listing: proto.BrowserListing{Path: "/srv"}})
	settle(t, m, m.Init())

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	require.True(t, m.Quitting())
	require.Equal(t, dialog.ServerMenuSelection{}, m.Selection())
	require.NotNil(t, cmd)
	require.IsType(t, tea.QuitMsg{}, cmd())
}

func TestViewBeforeInitDoesNotPanic(t *testing.T) {
	m := New(t.Context(), &fakeMenuClient{})
	require.NotPanics(t, func() {
		view := m.View()
		require.True(t, view.AltScreen)
	})
}
