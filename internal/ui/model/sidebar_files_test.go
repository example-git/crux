package model

import (
	"image"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/history"
	"github.com/stretchr/testify/require"
)

func TestSidebarPreviewClicksAndResize(t *testing.T) {
	preview, err := NewPreview()
	require.NoError(t, err)
	opts := PreviewOptions{Instance: "sidebar-clicks", Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 120, Rows: 80}
	_, err = preview.Render(opts)
	require.NoError(t, err)
	header := func() *PreviewPoint {
		u := preview.ui
		logoHeight := 0
		if u.sidebarDrawLogo != "" {
			logoHeight = strings.Count(u.sidebarDrawLogo, "\n") + 1
		}
		return &PreviewPoint{X: u.layout.sidebar.Min.X + 2, Y: u.layout.sidebar.Min.Y + 1 + logoHeight + u.sidebarSectionHeaders["skills"]}
	}
	opts.Click = header()
	frame, err := preview.Render(opts)
	require.NoError(t, err)
	require.True(t, preview.ui.sidebarCollapsed["skills"])
	require.Contains(t, ansi.Strip(frame.Content), "▸ Skills")
	require.NotContains(t, ansi.Strip(frame.Content), "crux-config")
	expandedHeader := header()
	opts.Click, opts.Rows = nil, 25
	_, err = preview.Render(opts)
	require.NoError(t, err)
	require.True(t, preview.ui.sidebarCollapsed["skills"])
	opts.Rows, opts.Click = 80, expandedHeader
	frame, err = preview.Render(opts)
	require.NoError(t, err)
	require.False(t, preview.ui.sidebarCollapsed["skills"])
	require.Contains(t, ansi.Strip(frame.Content), "crux-config")
	opts.Click = header()
	opts.Click.X = preview.ui.layout.sidebar.Min.X
	_, err = preview.Render(opts)
	require.NoError(t, err)
	require.False(t, preview.ui.sidebarCollapsed["skills"])
}

func TestSidebarFullContentsPriorityAndNativeCollapse(t *testing.T) {
	preview, err := NewPreview()
	require.NoError(t, err)
	_, err = preview.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 120, Rows: 100})
	require.NoError(t, err)
	u := preview.ui
	for _, name := range []string{"extra-a", "extra-b", "extra-c", "extra-d"} {
		mcp := u.mcpStates["dummy-devtools"]
		mcp.Name = name
		u.mcpStates[name] = mcp
		u.com.Config().MCP[name] = u.com.Config().MCP["dummy-devtools"]
		lsp := u.lspStates["dummy-gopls"]
		lsp.Name = name + "-lsp"
		u.lspStates[lsp.Name] = lsp
	}
	for _, name := range []string{"first.go", "second.go", "third.go", "fourth.go", "fifth.go", "sixth.go"} {
		u.sessionFiles = append(u.sessionFiles, SessionFile{FirstVersion: history.File{Path: "/workspace/" + name}, Additions: 1})
	}
	u.updateSidebarScrollState()
	full := ansi.Strip(u.sidebarContent)
	require.Contains(t, full, "sixth.go")
	require.NotContains(t, full, "…and")
	require.Contains(t, full, "Skills")
	require.Contains(t, full, "extra-d")
	require.Contains(t, full, "extra-d-lsp")
	for _, item := range u.skillStatusItems() {
		require.Contains(t, full, item.name)
	}
	click := func(line int) {
		logoHeight := 0
		if u.sidebarDrawLogo != "" {
			logoHeight = strings.Count(u.sidebarDrawLogo, "\n") + 1
		}
		u.Update(tea.MouseClickMsg(tea.Mouse{X: u.layout.sidebar.Min.X + 2, Y: u.layout.sidebar.Min.Y + 1 + logoHeight + line, Button: uv.MouseLeft}))
	}
	for _, id := range []string{"model", "session", "lsp", "mcp", "skills"} {
		line, exists := u.sidebarSectionHeaders[id]
		require.True(t, exists, id)
		click(line)
		require.True(t, u.sidebarCollapsed[id], id)
		require.Contains(t, ansi.Strip(u.sidebarContent), "▸")
		click(u.sidebarSectionHeaders[id])
		require.False(t, u.sidebarCollapsed[id], id)
	}
	click(u.sidebarFilesHeaderLine)
	require.True(t, u.sidebarFilesCollapsed)
	require.NotContains(t, ansi.Strip(u.sidebarContent), "sixth.go")
	click(u.sidebarFilesHeaderLine)
	require.False(t, u.sidebarFilesCollapsed)
	require.Contains(t, ansi.Strip(u.sidebarContent), "sixth.go")
	original := u.layout.sidebar
	u.layout.sidebar.Max.Y = u.layout.sidebar.Min.Y + 12
	u.updateSidebarScrollState()
	compact := ansi.Strip(u.sidebarContent)
	require.Contains(t, compact, "Model / Context")
	require.Contains(t, compact, "Modified Files")
	require.NotContains(t, compact, "Skills")
	require.NotContains(t, compact, "MCPs")
	modelLines := strings.Split(ansi.Strip(u.modelInfo(u.sidebarContentWidth)), "\n")
	require.Contains(t, compact, strings.TrimSpace(modelLines[len(modelLines)-1]))
	require.LessOrEqual(t, u.sidebarTotalLines, u.sidebarContentHeight)
	u.layout.sidebar = original
	u.updateSidebarScrollState()
	require.Equal(t, full, ansi.Strip(u.sidebarContent))
	click(u.sidebarSectionHeaders["skills"])
	u.layout.sidebar.Max.Y = u.layout.sidebar.Min.Y + 12
	u.updateSidebarScrollState()
	u.layout.sidebar = original
	u.updateSidebarScrollState()
	require.True(t, u.sidebarCollapsed["skills"])
	require.Contains(t, ansi.Strip(u.sidebarContent), "▸ Skills")
}

func TestSidebarSectionHasStableBounds(t *testing.T) {
	for _, content := range []string{"heading\n\nitem", "heading\n\n" + strings.Repeat("entry\n", 12), "heading\n\n\x1b[31m" + strings.Repeat("界", 40) + "\x1b[0m"} {
		section := sidebarSection(content, 30, 6)
		lines := strings.Split(section, "\n")
		require.Len(t, lines, min(6, strings.Count(content, "\n")+1))
		for _, line := range lines {
			require.LessOrEqual(t, ansi.StringWidth(line), 30)
		}
		if strings.Count(content, "\n") >= 6 {
			require.Contains(t, section, "… more")
		}
	}
	require.Empty(t, sidebarSection("heading", 30, 0))
}

func TestSidebarAdaptsSectionsToHeight(t *testing.T) {
	preview, err := NewPreview()
	require.NoError(t, err)
	_, err = preview.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 120, Rows: 80})
	require.NoError(t, err)
	u := preview.ui
	u.sessionFiles = nil
	u.lspStates = nil
	u.mcpStates = nil
	u.updateSidebarScrollState()
	content := ansi.Strip(u.sidebarContent)
	for _, heading := range []string{"Modified Files", "LSP", "MCP"} {
		require.NotContains(t, content, heading)
	}
	require.Contains(t, content, "Skills")
	u.sessionFiles = []SessionFile{{FirstVersion: history.File{Path: "/workspace/main.go"}, Additions: 1}}
	for _, height := range []int{25, 15, 5, 3, 2, 0, 80} {
		u.layout.sidebar.Max.Y = u.layout.sidebar.Min.Y + height
		u.sidebarOffset = 1000
		u.updateSidebarScrollState()
		require.GreaterOrEqual(t, u.sidebarContentHeight, 0)
		require.LessOrEqual(t, u.sidebarOffset, u.sidebarMaxOffsetVal)
		require.LessOrEqual(t, u.sidebarTotalLines, u.sidebarContentHeight)
		if height >= 15 {
			require.Contains(t, ansi.Strip(u.sidebarContent), "Modified Files")
		}
		if height <= 5 {
			require.NotContains(t, ansi.Strip(u.sidebarContent), "Skills")
		}
		screen := uv.NewScreenBuffer(120, 100)
		require.NotPanics(t, func() { u.drawSidebar(screen, u.layout.sidebar) })
	}
	u.sidebarOffset = 0
	u.updateSidebarScrollState()
	contentTop := u.layout.sidebar.Min.Y + 1 + strings.Count(u.sidebarDrawLogo, "\n") + 1
	require.True(t, u.handleSidebarFilesClick(tea.MouseClickMsg(tea.Mouse{X: u.layout.sidebar.Min.X + 2, Y: contentTop + u.sidebarFilesHeaderLine, Button: uv.MouseLeft})))
	require.True(t, u.sidebarFilesCollapsed)
}

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

func TestModifiedFilesSectionShowsCreatedEmptyFile(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.sessionFiles = []SessionFile{{
		FirstVersion:  history.File{Path: "/workspace/empty.txt"},
		LatestVersion: history.File{Path: "/workspace/empty.txt", Exists: true},
		Created:       true,
	}}

	rendered := stripANSI(ui.filesInfo("/workspace", 32, 10, true))
	require.Contains(t, rendered, "Modified Files")
	require.Contains(t, rendered, "1")
	require.Contains(t, rendered, "empty.txt")
	require.Contains(t, rendered, "new")
}

func TestSidebarModifiedFilesHeaderClickTogglesCollapsedState(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.layout.sidebar = image.Rect(10, 5, 42, 35)
	ui.sidebarDrawLogo = "logo\nlogo"
	ui.sidebarContent = "content"
	ui.sidebarFilesHeaderLine = 6
	ui.sidebarOffset = 2

	contentTop := ui.layout.sidebar.Min.Y + 3
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
