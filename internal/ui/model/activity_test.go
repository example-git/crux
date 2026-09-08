package model

import (
	"context"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type activityWorkspace struct {
	workspace.Workspace
	status proto.CodebaseIndexStatus
	err    error
}

func (w *activityWorkspace) CodebaseIndexStatus(context.Context) (proto.CodebaseIndexStatus, error) {
	return w.status, w.err
}

func TestActivityStatusLabel(t *testing.T) {
	t.Parallel()

	require.Equal(t, "indexing 12/40", activityStatusLabel(proto.CodebaseIndexStatus{
		State:          "indexing",
		FilesProcessed: 12,
		FilesTotal:     40,
	}))
	require.Equal(t, "indexing embedding  memory consolidating", activityStatusLabel(proto.CodebaseIndexStatus{
		State:          "indexing",
		Stage:          "embedding",
		MemoryActivity: "consolidating",
	}))
	require.Equal(t, "index ready  memory updating", activityStatusLabel(proto.CodebaseIndexStatus{
		State:          "ready",
		MemoryActivity: "updating",
	}))
	require.Equal(t, "index ready", activityStatusLabel(proto.CodebaseIndexStatus{State: "ready"}))
	require.Equal(t, "index ready, refreshing 12/40", activityStatusLabel(proto.CodebaseIndexStatus{State: "indexing", Serving: true, FilesProcessed: 12, FilesTotal: 40}))
}

func TestActivityRefreshFetchesOffThreadAndPollsWhileActive(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.com.Workspace = &activityWorkspace{status: proto.CodebaseIndexStatus{
		State:          "indexing",
		FilesProcessed: 4,
		FilesTotal:     10,
		MemoryActivity: "updating",
	}}

	cmd := ui.requestActivityRefresh()
	require.NotNil(t, cmd)
	require.True(t, ui.activityFetchInFlight)
	msg, ok := cmd().(activityStatusMsg)
	require.True(t, ok)
	next := ui.applyActivityStatus(msg)
	require.False(t, ui.activityFetchInFlight)
	require.Equal(t, 4, ui.activityStatus.FilesProcessed)
	require.NotNil(t, next)
}

func TestActivityRefreshKeepsIdleBackstopAndPreservesStateOnError(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	next := ui.applyActivityStatus(activityStatusMsg{status: proto.CodebaseIndexStatus{State: "ready"}})
	require.NotNil(t, next)

	ui.activityStatus = proto.CodebaseIndexStatus{MemoryActivity: "updating"}
	next = ui.applyActivityStatus(activityStatusMsg{err: context.DeadlineExceeded})
	require.NotNil(t, next)
	require.Equal(t, "updating", ui.activityStatus.MemoryActivity)
}

func TestEditorAccentUsesProviderGradientWithThemeFallback(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	fallback := ui.com.Styles.Logo.TitleColorB
	require.Equal(t, fallback, ui.editorAccent())

	providerAccent := lipgloss.Color("#123456")
	ui.brand = &providerBrand{GradB: providerAccent}
	require.Equal(t, providerAccent, ui.editorAccent())
}

func TestEditorFrameKeepsStatusAtTopRight(t *testing.T) {
	t.Parallel()

	line := renderEditorFrameLine(30, "attachment", "memory updating", "─", lipgloss.Color("#123456"), lipgloss.Color("#151515"))
	require.Equal(t, 30, ansi.StringWidth(line))
	require.Equal(t, "attachment──── memory updating", ansi.Strip(line))
	require.True(t, strings.HasSuffix(ansi.Strip(line), "memory updating"))
}

func TestEditorFrameUsesExactEditorWidthWithAndWithoutSidebar(t *testing.T) {
	t.Parallel()

	for _, compact := range []bool{false, true} {
		ui := newTestUI()
		ui.state = uiChat
		ui.isCompact = compact
		ui.activityStatus = proto.CodebaseIndexStatus{State: "ready"}
		layout := ui.generateLayout(120, 40)
		line := strings.Split(ui.renderEditorView(layout.editor.Dx()), "\n")[0]

		require.Equal(t, layout.editor.Dx(), ansi.StringWidth(line))
		require.True(t, strings.HasSuffix(ansi.Strip(line), "index ready╮"))
	}
}

func TestEditorOutlineSurvivesGrowthAndShrink(t *testing.T) {
	for _, width := range []int{65, 120} {
		for _, height := range []int{15, 20, 25, 45} {
			preview, err := NewPreview()
			require.NoError(t, err)
			_, err = preview.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: width, Rows: height})
			require.NoError(t, err)
			u := preview.ui
			for _, value := range []string{"short", strings.Repeat("line\n", 20), "short", strings.Repeat("wrapped text ", 200), ""} {
				previous := u.textarea.Height()
				u.textarea.SetValue(value)
				u.textarea.MoveToEnd()
				u.handleTextareaHeightChange(previous)
				screen := uv.NewScreenBuffer(width, height)
				u.Draw(screen, screen.Bounds())
				area := u.layout.editor
				require.Equal(t, u.textarea.Height()+2, area.Dy(), "width=%d height=%d input=%d", width, height, len(value))
				require.Equal(t, "╭", screen.CellAt(area.Min.X, area.Min.Y).Content)
				require.Equal(t, "╮", screen.CellAt(area.Max.X-1, area.Min.Y).Content)
				require.Equal(t, "╰", screen.CellAt(area.Min.X, area.Max.Y-1).Content)
				require.Equal(t, "╯", screen.CellAt(area.Max.X-1, area.Max.Y-1).Content)
				for y := area.Min.Y + 1; y < area.Max.Y-1; y++ {
					require.Equal(t, "│", screen.CellAt(area.Min.X, y).Content)
					require.Equal(t, "│", screen.CellAt(area.Max.X-1, y).Content)
				}
			}
		}
	}
}

func TestEditorOutlineSurvivesNativeInputAndResize(t *testing.T) {
	preview, err := NewPreview()
	require.NoError(t, err)
	_, err = preview.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 120, Rows: 45})
	require.NoError(t, err)
	u := preview.ui
	u.focus = uiFocusEditor
	u.textarea.Focus()
	_, _ = u.Update(openEditorMsg{Text: ""})
	value := strings.Repeat("wrapped input text ", 30)
	_, _ = u.Update(tea.PasteMsg{Content: value})
	require.Equal(t, value, u.textarea.Value())
	for _, size := range [][2]int{{65, 15}, {120, 45}, {45, 20}, {160, 25}, {65, 15}} {
		_, _ = u.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		screen := uv.NewScreenBuffer(size[0], size[1])
		u.Draw(screen, screen.Bounds())
		area := u.layout.editor
		require.Equal(t, u.textarea.Height()+2, area.Dy())
		require.Equal(t, "╭", screen.CellAt(area.Min.X, area.Min.Y).Content)
		require.Equal(t, "╮", screen.CellAt(area.Max.X-1, area.Min.Y).Content)
		require.Equal(t, "╰", screen.CellAt(area.Min.X, area.Max.Y-1).Content)
		require.Equal(t, "╯", screen.CellAt(area.Max.X-1, area.Max.Y-1).Content)
		require.Equal(t, value, u.textarea.Value())
	}
	_, _ = u.Update(openEditorMsg{Text: ""})
	screen := uv.NewScreenBuffer(u.width, u.height)
	u.Draw(screen, screen.Bounds())
	require.Equal(t, TextareaMinHeight+2, u.layout.editor.Dy())
	require.Empty(t, u.textarea.Value())
	require.Equal(t, "╯", screen.CellAt(u.layout.editor.Max.X-1, u.layout.editor.Max.Y-1).Content)
}

func TestEditorRoundedFramePreservesBodyAndCursor(t *testing.T) {
	t.Parallel()

	for _, width := range []int{40, 65, 120, 160} {
		for _, compact := range []bool{false, true} {
			preview, err := NewPreview()
			require.NoError(t, err)
			_, err = preview.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: max(45, width), Rows: 45, Compact: compact})
			require.NoError(t, err)
			u := preview.ui
			u.width = width
			u.textarea.SetValue("first line\nsecond line")
			u.updateLayoutAndSize()
			view := u.renderEditorView(u.layout.editor.Dx())
			lines := strings.Split(view, "\n")
			w := u.layout.editor.Dx()
			screen := uv.NewScreenBuffer(w, len(lines))
			uv.NewStyledString(view).Draw(screen, screen.Bounds())
			inside := color.RGBAModel.Convert(u.com.Styles.Background)
			for y, line := range lines {
				require.Equal(t, w, ansi.StringWidth(line))
				for _, x := range []int{0, w - 1} {
					require.Equal(t, inside, color.RGBAModel.Convert(screen.CellAt(x, y).Style.Bg))
				}
				if y > 0 && y < len(lines)-1 {
					require.Equal(t, "│", screen.CellAt(0, y).Content)
					require.Equal(t, "│", screen.CellAt(w-1, y).Content)
					require.Equal(t, inside, color.RGBAModel.Convert(screen.CellAt(w-2, y).Style.Bg))
				}
			}
			for x := 1; x < w-1; x++ {
				require.Equal(t, "─", screen.CellAt(x, 0).Content)
				require.Equal(t, "─", screen.CellAt(x, len(lines)-1).Content)
				require.Equal(t, color.RGBAModel.Convert(u.editorAccent()), color.RGBAModel.Convert(screen.CellAt(x, 0).Style.Fg))
			}
			require.Equal(t, "╭", screen.CellAt(0, 0).Content)
			require.Equal(t, "╮", screen.CellAt(w-1, 0).Content)
			require.Equal(t, "╰", screen.CellAt(0, len(lines)-1).Content)
			require.Equal(t, "╯", screen.CellAt(w-1, len(lines)-1).Content)
			require.Len(t, lines, u.textarea.Height()+2)
			require.Equal(t, len(lines), u.layout.editor.Dy())
			require.Contains(t, ansi.Strip(lines[1]), "first line")
			require.Contains(t, ansi.Strip(view), "first line")
			require.Contains(t, ansi.Strip(view), "second line")
			require.Equal(t, "first line\nsecond line", u.textarea.Value())
			local := u.textarea.Cursor()
			canvas := uv.NewScreenBuffer(u.width, u.height)
			cursor := u.Draw(canvas, canvas.Bounds())
			require.NotNil(t, cursor)
			require.Equal(t, local.X+u.layout.editor.Min.X+1, cursor.X)
			require.Equal(t, local.Y+u.layout.editor.Min.Y+1, cursor.Y)
		}
	}
}

func TestEditorBodyUsesChatBackgroundAndFillsWidth(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	require.Equal(t, ui.com.Styles.Background, ui.com.Styles.Editor.Background)

	body := paintEditorBody("one\ntwo", 12, ui.com.Styles.Editor.Background)
	lines := strings.Split(body, "\n")
	require.Len(t, lines, 2)
	require.Equal(t, 12, ansi.StringWidth(lines[0]))
	require.Equal(t, 12, ansi.StringWidth(lines[1]))
}

func TestRenderEditorViewIntegratesFrameBackgroundAndNotifier(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.activityStatus = proto.CodebaseIndexStatus{
		State:          "indexing",
		FilesProcessed: 8,
		FilesTotal:     20,
		MemoryActivity: "updating",
	}
	view := ui.renderEditorView(44)
	lines := strings.Split(view, "\n")
	require.Len(t, lines, ui.textarea.Height()+2)
	for _, line := range lines {
		require.Equal(t, 44, ansi.StringWidth(line))
	}
	require.Contains(t, ansi.Strip(lines[0]), "indexing 8/20  memory updating")
	require.Equal(t, "╰"+strings.Repeat("─", 42)+"╯", ansi.Strip(lines[len(lines)-1]))
	require.True(t, strings.HasPrefix(ansi.Strip(lines[0]), "╭"))
	require.True(t, strings.HasSuffix(ansi.Strip(lines[0]), "╮"))
	for _, line := range lines[1 : len(lines)-1] {
		require.True(t, strings.HasPrefix(ansi.Strip(line), "│"))
		require.True(t, strings.HasSuffix(ansi.Strip(line), "│"))
	}
}
