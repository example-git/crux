package model

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/stretchr/testify/require"
)

func TestStatusHelpUsesExtraTerminalSpace(t *testing.T) {
	for _, width := range []int{65, 120, 160, 240, 320} {
		p, err := NewPreview()
		require.NoError(t, err)
		_, err = p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: width, Rows: 40})
		require.NoError(t, err)
		m := p.ui
		m.status.ClearInfoMsg()
		for _, code := range []rune{tea.KeyLeft, tea.KeyRight} {
			m.handleKeyPressMsg(tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl})
			screen := uv.NewScreenBuffer(width, 40)
			m.Draw(screen, screen.Bounds())
			style := m.com.Styles.Status.Help
			available := width - style.GetHorizontalFrameSize()
			view := m.status.renderShortHelp(available)
			require.LessOrEqual(t, ansi.StringWidth(view), available)
			require.NotContains(t, view, "\n")
			if width >= 240 {
				old := dialog.ShortHelpLine(&m.status.help, m.ShortHelp(), available)
				require.Greater(t, ansi.StringWidth(view), ansi.StringWidth(old))
				require.Contains(t, ansi.Strip(view), m.keyMap.Sessions.Help().Desc)
			}
			expected := uv.NewScreenBuffer(width, 1)
			uv.NewStyledString(style.Render(view)).Draw(expected, expected.Bounds())
			for x := 0; x < ansi.StringWidth(style.Render(view)); x++ {
				require.Equal(t, expected.CellAt(x, 0), screen.CellAt(x, m.layout.status.Min.Y), "width=%d compact=%t x=%d", width, m.isCompact, x)
			}
		}
	}
}

func TestNotificationDrawSpansTerminalWidth(t *testing.T) {
	for _, width := range []int{65, 120, 160} {
		p, err := NewPreview()
		require.NoError(t, err)
		_, err = p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: width, Rows: 40})
		require.NoError(t, err)
		m := p.ui
		for _, code := range []rune{tea.KeyLeft, tea.KeyRight} {
			m.handleKeyPressMsg(tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl})
			for _, msg := range []util.InfoMsg{
				{Type: util.InfoTypeInfo, Msg: "Copied"},
				{Type: util.InfoTypeSuccess, Msg: "Saved"},
				{Type: util.InfoTypeWarn, Msg: "Warning"},
				{Type: util.InfoTypeError, Msg: strings.Repeat("Long error ", 40)},
				{Type: util.InfoTypeUpdate, Msg: "Update available"},
			} {
				m.status.SetInfoMsg(msg)
				screen := uv.NewScreenBuffer(width, 40)
				m.Draw(screen, screen.Bounds())
				require.Equal(t, 0, m.layout.status.Min.X)
				require.Equal(t, width, m.layout.status.Max.X)
				expected := uv.NewScreenBuffer(width, 1)
				uv.NewStyledString(m.status.renderInfo(width)).Draw(expected, expected.Bounds())
				for x := 0; x < width; x++ {
					require.Equal(t, expected.CellAt(x, 0), screen.CellAt(x, m.layout.status.Min.Y), "width=%d compact=%t type=%v x=%d", width, m.isCompact, msg.Type, x)
				}
			}
		}
	}
}

func TestStatusInfoFillsButDoesNotExceedViewport(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	status := NewStatus(&common.Common{Styles: &sty}, nil)
	status.SetInfoMsg(util.NewInfoMsg("Copied"))

	view := status.renderInfo(120)
	require.Contains(t, ansi.Strip(view), "Copied")
	require.Equal(t, 120, ansi.StringWidth(view))
}

type boundedStatusScreen struct {
	uv.ScreenBuffer
	bounds uv.Rectangle
}

func (s boundedStatusScreen) Bounds() uv.Rectangle {
	return s.bounds
}

func TestStatusDrawUsesVisibleScreenBounds(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	status := NewStatus(&common.Common{Styles: &sty}, nil)
	status.SetHideHelp(true)
	status.SetInfoMsg(util.NewInfoMsg("Copied"))
	screen := boundedStatusScreen{
		ScreenBuffer: uv.ScreenBuffer{
			RenderBuffer: uv.NewRenderBuffer(120, 1),
			Method:       ansi.GraphemeWidth,
		},
		bounds: uv.Rect(0, 0, 20, 1),
	}

	status.Draw(screen, uv.Rect(0, 0, 120, 1))

	require.False(t, screen.CellAt(19, 0).IsZero())
	for x := 20; x < 120; x++ {
		cell := screen.CellAt(x, 0)
		untouched := cell == nil || cell.IsZero() ||
			cell.Content == " " && cell.Width == 1 && cell.Style == (uv.Style{})
		require.True(t, untouched, "cell %d", x)
	}
}

func TestStatusInfoStaysWithinViewport(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	status := NewStatus(&common.Common{Styles: &sty}, nil)
	status.SetInfoMsg(util.InfoMsg{
		Type: util.InfoTypeError,
		Msg:  strings.Repeat("message ", 40),
	})

	for width := 0; width <= 40; width++ {
		view := status.renderInfo(width)
		require.LessOrEqual(t, ansi.StringWidth(view), width, "width %d", width)
	}
}

func TestStatusInfoCollapsesMultilineMessage(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	status := NewStatus(&common.Common{Styles: &sty}, nil)
	status.SetInfoMsg(util.NewInfoMsg("first\nsecond"))

	view := ansi.Strip(status.renderInfo(80))
	require.NotContains(t, view, "\n")
	require.Contains(t, view, "first second")
}
