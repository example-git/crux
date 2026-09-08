package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestSingleNonblankThinkingLineStaysVisible(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, width := range []int{40, 80} {
		for _, answer := range []string{"", "Answer"} {
			for _, thought := range []string{"One thought", "\n \t\nOne thought\n\n", "START " + strings.Repeat("wrapped thinking ", 100) + " END"} {
				item := NewAssistantMessageItem(&sty, thinkingMessage("single", thought, answer)).(*AssistantMessageItem)
				for _, mode := range []thinkingViewMode{thinkingCollapsed, thinkingTailWindow, thinkingFullExpanded} {
					item.thinkingViewMode = mode
					item.clearCache()
					before := item.Render(width)
					plain := ansi.Strip(before)
					require.NotContains(t, plain, "Expand Thoughts")
					require.NotContains(t, plain, "hidden")
					if strings.Contains(thought, "START") {
						require.Contains(t, plain, "START")
						require.Contains(t, plain, "END")
					} else {
						require.Contains(t, plain, "One thought")
						require.Equal(t, 1, item.thinkingBoxHeight)
					}
					require.False(t, item.ToggleExpanded())
					require.False(t, item.HandleMouseClick(ansi.MouseLeft, 4, 0))
					require.Equal(t, before, item.Render(width))
				}
			}
		}
	}
}

func TestBlankThinkingRowsAreSkipped(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, answer := range []string{"", "Answer"} {
		item := NewAssistantMessageItem(&sty, thinkingMessage("blanks", "\n \t\n\n", answer)).(*AssistantMessageItem)
		require.Empty(t, item.cachedThinking(80))
		require.Zero(t, item.thinkingBoxHeight)
		require.False(t, item.ToggleExpanded())
		require.False(t, item.JoinPrevious())
		require.False(t, item.HandleMouseClick(ansi.MouseLeft, 4, 0))
		require.Nil(t, item.SetMessage(thinkingMessage("blanks", "First\n\n \t\nSecond", answer)))
		require.Contains(t, ansi.Strip(item.Render(80)), "Expand Thoughts")
		require.True(t, item.ToggleExpanded())
		item.Render(80)
		for _, line := range strings.Split(ansi.Strip(item.thinkingSec.out), "\n") {
			index := strings.IndexAny(line, "╭╰├")
			if index >= 0 {
				require.NotEmpty(t, strings.TrimSpace(strings.TrimLeft(line[index:], "╭╰├─ ")))
			}
		}
		require.Nil(t, item.SetMessage(thinkingMessage("blanks", "\nOnly one\n \n", answer)))
		require.Contains(t, ansi.Strip(item.Render(80)), "Only one")
		require.NotContains(t, ansi.Strip(item.Render(80)), "Expand Thoughts")
		require.False(t, item.ToggleExpanded())
		require.Nil(t, item.SetMessage(thinkingMessage("blanks", "\n\n", answer)))
		require.Empty(t, item.cachedThinking(80))
		require.Zero(t, item.thinkingBoxHeight)
	}
}
