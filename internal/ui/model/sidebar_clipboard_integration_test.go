package model

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/clipboard"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

// This opt-in test writes the native clipboard. Its caller must preserve and
// restore all clipboard formats; ordinary unit tests never mutate it.
func TestSidebarSessionIDNativeClipboard(t *testing.T) {
	if os.Getenv("CRUX_VERIFY_SID_CLIPBOARD") != "1" {
		t.Skip("requires a caller-managed native clipboard snapshot")
	}
	require.NoError(t, clipboard.Init())
	u := sidebarSessionPreview(t, map[string]any{"directory": "/short"}, 80)
	m := sidebarClipboardProgram{ui: u}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(&output), tea.WithoutSignalHandler(), tea.WithoutRenderer()).Run()
	require.NoError(t, err)
	id := session.ShortID(u.session.ID)
	actual, err := clipboard.Read(clipboard.FormatText)
	require.NoError(t, err)
	require.Equal(t, id, string(actual))
	require.Contains(t, output.String(), ansi.SetSystemClipboard(id))
	require.Empty(t, u.sidebarSession.selectedID())
}

type sidebarClipboardProgram struct{ ui *UI }

func (m sidebarClipboardProgram) Init() tea.Cmd {
	r := m.ui.sidebarSession.idRect
	return tea.Sequence(
		func() tea.Msg { return tea.MouseClickMsg{X: r.Min.X, Y: r.Min.Y, Button: uv.MouseLeft} },
		func() tea.Msg { return tea.MouseMotionMsg{X: r.Max.X, Y: r.Min.Y, Button: uv.MouseLeft} },
		func() tea.Msg { return tea.MouseReleaseMsg{X: r.Max.X, Y: r.Min.Y, Button: uv.MouseLeft} },
	)
}

func (m sidebarClipboardProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg, copySidebarSessionIDMsg:
		_, cmd := m.ui.Update(msg)
		return m, cmd
	case sidebarSessionIDCopiedMsg:
		m.ui.Update(msg)
		return m, tea.Quit
	}
	return m, nil
}

func (m sidebarClipboardProgram) View() tea.View { return m.ui.View() }
