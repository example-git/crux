package model

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/stretchr/testify/require"
)

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
