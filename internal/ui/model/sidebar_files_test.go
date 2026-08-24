package model

import (
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/internal/history"
	"github.com/stretchr/testify/require"
)

func TestModifiedFilesSectionCollapsesWithoutAffectingCompactDetails(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.sessionFiles = []SessionFile{{
		FirstVersion: history.File{Path: "/workspace/main.go"},
		Additions:    3,
		Deletions:    1,
	}}

	expanded := stripANSI(ui.filesInfo("/workspace", 32, 10, true))
	require.Contains(t, expanded, "▾ Modified Files")
	require.Contains(t, expanded, "main.go")

	ui.sidebarFilesCollapsed = true
	collapsed := stripANSI(ui.filesInfo("/workspace", 32, 10, true))
	require.Contains(t, collapsed, "▸ Modified Files")
	require.NotContains(t, collapsed, "main.go")

	compactDetails := stripANSI(ui.filesInfo("/workspace", 32, 10, false))
	require.Contains(t, compactDetails, "main.go")
}

func TestSidebarModifiedFilesHeaderClickTogglesCollapsedState(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.layout.sidebar = image.Rect(10, 5, 42, 35)
	ui.sidebarDrawLogo = "logo\nlogo"
	ui.sidebarContent = "content"
	ui.sidebarFilesHeaderLine = 6
	ui.sidebarOffset = 2

	contentTop := ui.layout.sidebar.Min.Y + 2
	require.True(t, ui.handleSidebarFilesClick(tea.MouseClickMsg(tea.Mouse{
		X:      12,
		Y:      contentTop + ui.sidebarFilesHeaderLine - ui.sidebarOffset,
		Button: uv.MouseLeft,
	})))
	require.True(t, ui.sidebarFilesCollapsed)

	require.False(t, ui.handleSidebarFilesClick(tea.MouseClickMsg(tea.Mouse{
		X:      12,
		Y:      contentTop + ui.sidebarFilesHeaderLine - ui.sidebarOffset + 1,
		Button: uv.MouseLeft,
	})))
	require.True(t, ui.sidebarFilesCollapsed)
}
