package model

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

func TestTaskPanelRestoresDraftAndRejectsClosedReplies(t *testing.T) {
	m := newBusyUI(&countingWorkspace{ready: true})
	m.textarea.SetValue("preserved draft")
	cmd := m.openTasksDialog()
	old := m.taskPanel
	require.NotNil(t, cmd)
	require.False(t, m.dialog.HasDialogs())
	m.handleTaskPanelMsg(tea.KeyPressMsg{Code: 'x', Text: "x"})
	require.Equal(t, "preserved draft", m.textarea.Value())
	m.handleTaskPanelMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Nil(t, m.taskPanel)
	require.Equal(t, "preserved draft", m.textarea.Value())
	require.True(t, m.textarea.Focused())
	m.openTasksDialog()
	current := m.taskPanel
	m.Update(taskPanelReplyMsg{panel: old, msg: tea.KeyPressMsg{Code: tea.KeyEscape}})
	require.Same(t, current, m.taskPanel)
}

func TestSidebarEnabledByDefault(t *testing.T) {
	for _, compact := range []bool{false, true} {
		p, err := NewPreview()
		require.NoError(t, err)
		_, err = p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 120, Rows: 40})
		require.NoError(t, err)
		p.ui.com.Config().Options.TUI.CompactMode = compact
		m := New(p.ui.com, "", false, "")
		m.session = p.ui.session
		m.setState(uiChat, uiFocusEditor)
		for _, size := range [][2]int{{65, 25}, {120, 40}, {160, 45}, {65, 25}} {
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			require.Equal(t, compact, m.isCompact)
			if compact {
				require.True(t, m.layout.sidebar.Empty())
			} else {
				require.Equal(t, 34, m.layout.sidebar.Dx())
				screen := uv.NewScreenBuffer(size[0], size[1])
				m.Draw(screen, screen.Bounds())
				require.Equal(t, "╭", screen.CellAt(m.layout.sidebar.Min.X, m.layout.sidebar.Min.Y).Content)
			}
		}
	}
}

func TestSidebarDirectionalKeysPreserveTaskPanelAndDraft(t *testing.T) {
	for _, width := range []int{65, 120, 160} {
		for _, modal := range []string{"none", "tasks", "task-detail"} {
			p, err := NewPreview()
			require.NoError(t, err)
			_, err = p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: width, Rows: 40, Modal: modal})
			require.NoError(t, err)
			m := p.ui
			m.textarea.SetValue("preserved draft")
			panel := m.taskPanel
			for _, code := range []rune{tea.KeyRight, tea.KeyRight, tea.KeyLeft, tea.KeyLeft, tea.KeyRight} {
				m.handleKeyPressMsg(tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl})
				require.Equal(t, code == tea.KeyRight, m.isCompact)
				require.Equal(t, "preserved draft", m.textarea.Value())
				require.Same(t, panel, m.taskPanel)
				if code == tea.KeyLeft {
					require.Equal(t, 1, m.layout.sidebar.Min.Y)
					require.Equal(t, width-1, m.layout.sidebar.Max.X)
					screen := uv.NewScreenBuffer(width, 40)
					m.Draw(screen, screen.Bounds())
					area := m.layout.sidebar
					require.Equal(t, "╭", screen.CellAt(area.Min.X, area.Min.Y).Content)
					require.Equal(t, "╮", screen.CellAt(area.Max.X-1, area.Min.Y).Content)
					require.Equal(t, "╰", screen.CellAt(area.Min.X, area.Max.Y-1).Content)
					require.Equal(t, "╯", screen.CellAt(area.Max.X-1, area.Max.Y-1).Content)
					for x := area.Min.X + 1; x < area.Max.X-1; x++ {
						require.Equal(t, "─", screen.CellAt(x, area.Min.Y).Content)
						require.Equal(t, color.RGBAModel.Convert(m.editorAccent()), color.RGBAModel.Convert(screen.CellAt(x, area.Min.Y).Style.Fg))
						require.Equal(t, color.RGBAModel.Convert(m.com.Styles.Sidebar.Background), color.RGBAModel.Convert(screen.CellAt(x, area.Min.Y).Style.Bg))
					}
					for y := area.Min.Y + 1; y < area.Max.Y-1; y++ {
						require.Equal(t, "│", screen.CellAt(area.Min.X, y).Content)
						require.Equal(t, "│", screen.CellAt(area.Max.X-1, y).Content)
					}
					for y := area.Min.Y; y < area.Max.Y; y++ {
						require.Equal(t, " ", screen.CellAt(area.Max.X, y).Content)
					}
					boundary := m.layout.editor.Min.Y
					if modal == "task-detail" {
						boundary = m.layout.pills.Min.Y
					}
					require.Equal(t, boundary-1, area.Max.Y)
					for x := area.Min.X; x < area.Max.X; x++ {
						require.Equal(t, " ", screen.CellAt(x, area.Max.Y).Content)
					}
					if modal == "none" {
						require.Equal(t, "─", screen.CellAt(area.Min.X, m.layout.editor.Min.Y).Content)
					}
					require.Greater(t, m.layout.editor.Max.X, area.Min.X)
					require.Equal(t, 34, area.Dx())
					require.Equal(t, 30, m.sidebarContentWidth)
					for offset, glyph := range []rune(" ctrl+left show ctrl+right hide ") {
						cell := screen.CellAt(area.Min.X+1+offset, area.Max.Y-1)
						require.Equal(t, string(glyph), cell.Content, "width=%d modal=%s offset=%d area=%v", width, modal, offset, area)
						require.Equal(t, color.RGBA{A: 255}, color.RGBAModel.Convert(cell.Style.Fg))
						require.Equal(t, color.RGBAModel.Convert(m.editorAccent()), color.RGBAModel.Convert(cell.Style.Bg))
					}
				}
			}
		}
	}
}

func TestTaskPanelLayoutAndNativeBack(t *testing.T) {
	for _, size := range [][2]int{{65, 45}, {120, 45}, {65, 25}} {
		p, err := NewPreview()
		require.NoError(t, err)
		_, err = p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: size[0], Rows: size[1], Modal: "task-detail"})
		require.NoError(t, err)
		m := p.ui
		require.NotNil(t, m.taskPanel)
		require.InDelta(t, max(9, float64(size[1])*0.3), m.layout.editor.Dy(), 1)
		require.LessOrEqual(t, m.layout.main.Max.Y, m.layout.editor.Min.Y)
		require.False(t, m.dialog.HasDialogs())
		m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
		require.NotNil(t, m.taskPanel)
		m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
		require.Nil(t, m.taskPanel)
	}
}

func TestPreviewTaskPanelNativeSelection(t *testing.T) {
	p, err := NewPreview()
	require.NoError(t, err)
	o := PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 120, Rows: 45, Modal: "tasks"}
	_, err = p.Render(o)
	require.NoError(t, err)
	o.Click = &PreviewPoint{X: p.ui.layout.editor.Min.X + 2, Y: p.ui.layout.editor.Min.Y + 4}
	_, err = p.Render(o)
	require.NoError(t, err)
	require.NotNil(t, p.ui.taskPanel)
	require.Equal(t, "back", p.ui.taskPanel.ShortHelp()[0].Help().Desc)
}

func TestTaskPanelSurfaceAndMetadata(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, modal := range []string{"tasks", "task-detail"} {
			p, err := NewPreview()
			require.NoError(t, err)
			_, err = p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 160, Rows: 45, Compact: compact, Modal: modal})
			require.NoError(t, err)
			m := p.ui
			screen := uv.NewScreenBuffer(160, 45)
			m.Draw(screen, uv.Rect(0, 0, 160, 45))
			right := 160
			require.Equal(t, right, m.layout.editor.Max.X)
			require.Equal(t, 45, m.layout.editor.Max.Y)
			background := m.com.Styles.TaskPanel.ControlsBackground
			if modal == "tasks" {
				background = m.com.Styles.Background
			}
			for x := 0; x < right; x++ {
				cell := screen.CellAt(x, 44)
				require.NotNil(t, cell)
				require.Equal(t, color.RGBAModel.Convert(background), color.RGBAModel.Convert(cell.Style.Bg), "bottom cell %d, compact=%t modal=%s", x, compact, modal)
			}
			for y := m.layout.editor.Min.Y; y < 44; y++ {
				edgeBackground := m.com.Styles.PanelBackground
				if modal == "tasks" {
					edgeBackground = m.com.Styles.Background
				}
				require.Equal(t, color.RGBAModel.Convert(edgeBackground), color.RGBAModel.Convert(screen.CellAt(right-1, y).Style.Bg), "right edge row %d", y)
			}
			if modal == "task-detail" {
				accent := color.RGBAModel.Convert(m.editorAccent())
				for _, y := range []int{m.layout.editor.Min.Y, m.layout.editor.Max.Y - 2} {
					for x := 0; x < right; x++ {
						cell := screen.CellAt(x, y)
						if y == m.layout.editor.Min.Y || cell.Content == "▀" {
							require.Equal(t, accent, color.RGBAModel.Convert(cell.Style.Fg), "frame cell %d,%d compact=%t", x, y, compact)
						}
					}
				}
				require.NotEmpty(t, m.taskPanel.PanelInfoLines())
				require.NotContains(t, m.pillsView, "To-Do")
				require.Contains(t, m.pillsView, m.taskPanel.PanelInfoLines()[1])
				handled, _ := m.routeTaskPanelInput(tea.MouseClickMsg{X: m.layout.pills.Min.X, Y: m.layout.pills.Min.Y})
				require.True(t, handled)
				m.handleTaskPanelMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
				require.Empty(t, m.taskPanel.PanelInfoLines())
				require.Contains(t, m.pillsView, "To-Do")
			}
		}
	}
}

func TestTaskPanelCommandWrapperPreservesAsyncBatch(t *testing.T) {
	panel := &dialog.Tasks{}
	calls := 0
	cmd := wrapTaskPanelCmd(panel, tea.Batch(func() tea.Msg { calls++; return "one" }, func() tea.Msg { calls++; return "two" }))
	require.Zero(t, calls)
	batch := cmd().(tea.BatchMsg)
	require.Zero(t, calls)
	for _, child := range batch {
		reply := child().(taskPanelReplyMsg)
		require.Same(t, panel, reply.panel)
	}
	require.Equal(t, 2, calls)
}
