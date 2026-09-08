package diffview_test

import (
	"image/color"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/diffview"
	"github.com/stretchr/testify/require"
)

func TestWrappedSplitDiffFillsShorterSideBackground(t *testing.T) {
	for _, width := range []int{48, 49, 160} {
		for _, longBefore := range []bool{false, true} {
			before, after := "old", strings.Repeat("new ", 90)
			if longBefore {
				before, after = strings.Repeat("old ", 90), "new"
			}
			patch := "@@ -1 +1 @@\n-" + before + "\n+" + after + "\n"
			view := diffview.New().Patch(patch).Width(width).Split().Wrap(true).String()
			lines := strings.Split(view, "\n")
			screen := uv.NewScreenBuffer(width, len(lines))
			uv.NewStyledString(view).Draw(screen, screen.Bounds())
			require.Greater(t, len(lines), 3)
			for y := 2; y < len(lines); y++ {
				for x := 0; x < width; x++ {
					first := screen.CellAt(x, 1).Style.Bg
					actual := screen.CellAt(x, y).Style.Bg
					require.NotNil(t, first)
					require.NotNil(t, actual, "width=%d longBefore=%t x=%d y=%d", width, longBefore, x, y)
					require.Equal(t, color.RGBAModel.Convert(first), color.RGBAModel.Convert(actual), "width=%d longBefore=%t x=%d y=%d", width, longBefore, x, y)
				}
			}
		}
	}
}

func TestPatchPreservesHeaderLikeContentAndSourcePositions(t *testing.T) {
	patch := "--- a/first.txt\n+++ b/first.txt\n@@ -100,2 +200,2 @@ section\n--- old text\n+++ new text\n context\n--- a/second.txt\n+++ b/second.txt\n@@ -50 +60 @@\n-old\n+new\n"
	view := ansi.Strip(diffview.New().Patch(patch).Width(80).Wrap(true).String())
	require.Regexp(t, `(?m)^\s*100\s+- -- old text`, view)
	require.Regexp(t, `(?m)^\s*200\s+\+ \+\+ new text`, view)
	require.Regexp(t, `(?m)^\s*101\s+201\s+context`, view)
	require.Regexp(t, `(?m)^\s*50\s+- old`, view)
	require.Regexp(t, `(?m)^\s*60\s+\+ new`, view)
	require.Contains(t, view, "--- a/second.txt")
}

func TestWrappedPatchKeepsEveryCharacterAndContinuationGutters(t *testing.T) {
	for _, split := range []bool{false, true} {
		patch := "--- a/test.txt\n+++ b/test.txt\n@@ -100,2 +200,2 @@\n-" + strings.Repeat("界", 70) + "\n+" + strings.Repeat("語", 70) + "\n unchanged\n\\ No newline at end of file\n"
		diff := diffview.New().Patch(patch).Width(48).Wrap(true)
		if split {
			diff.Split()
		}
		view := ansi.Strip(diff.String())
		require.Equal(t, 70, strings.Count(view, "界"))
		require.Equal(t, 70, strings.Count(view, "語"))
		require.Contains(t, view, "unchanged")
		require.Contains(t, view, "No newline at end of file")
		require.NotRegexp(t, `(?m)^\s*102\s`, view)
		require.NotRegexp(t, `(?m)^\s*202\s`, view)
		for _, line := range strings.Split(view, "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), 48)
		}
	}
}
