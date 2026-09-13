package chat

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/anim"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestSingleNonblankThinkingLineStaysVisible(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, width := range []int{40, 80} {
		for _, thought := range []string{"One thought", "\n \t\nOne thought\n\n", "START " + strings.Repeat("wrapped thinking ", 100) + "END"} {
			msg := &message.Message{
				ID:   "single",
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.ReasoningContent{Thinking: thought, StartedAt: testStartedAt, FinishedAt: testFinishedAt},
					message.Finish{Reason: message.FinishReasonEndTurn, Time: testFinishTime},
				},
			}
			item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
			for _, mode := range []thinkingViewMode{thinkingCollapsed, thinkingTailWindow, thinkingFullExpanded} {
				item.thinkingViewMode = mode
				item.clearCache()
				rendered := item.cachedThinking(width)
				plain := ansi.Strip(rendered)
				require.NotContains(t, plain, "THOUGHTS ▾")
				require.NotContains(t, plain, "Thinking")
				require.NotContains(t, plain, "•")
				require.NotContains(t, plain, "hidden")
				lines := strings.Split(plain, "\n")
				require.True(t, strings.HasPrefix(strings.TrimSpace(lines[0]), "╭"))
				require.True(t, strings.HasSuffix(strings.TrimSpace(lines[0]), "╮"))
				require.True(t, strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "╰"))
				require.True(t, strings.HasSuffix(strings.TrimSpace(lines[len(lines)-1]), "╯"))
				frameWidth := ansi.StringWidth(strings.TrimSpace(lines[0]))
				for _, line := range lines[1 : len(lines)-1] {
					trimmed := strings.TrimSpace(line)
					require.True(t, strings.HasPrefix(trimmed, "│"), line)
					require.True(t, strings.HasSuffix(trimmed, "│"), line)
					require.Equal(t, frameWidth, ansi.StringWidth(trimmed), line)
				}
				if strings.Contains(thought, "START") {
					require.Contains(t, plain, "START")
					require.Contains(t, plain, "END")
					require.Greater(t, len(lines), 3)
				} else {
					require.Contains(t, plain, "One thought")
					require.Len(t, lines, 3)
				}
				require.Equal(t, lipgloss.Height(rendered), item.thinkingBoxHeight)
				require.False(t, item.ToggleExpanded())
				require.False(t, item.HandleMouseClick(ansi.MouseLeft, 4, 0))
			}
		}
	}
}

func TestSingleThinkingLineRemovesOuterEmphasisWrapper(t *testing.T) {
	sty := styles.CharmtonePantera()
	item := NewAssistantMessageItem(&sty, thinkingMessage("single-wrapped", "  **Wrapped *internal* thought**  ", "Answer")).(*AssistantMessageItem)
	rendered := ansi.Strip(item.cachedThinking(80))
	require.Contains(t, rendered, "Wrapped *internal* thought")
	require.NotContains(t, rendered, "**Wrapped *internal* thought**")
	require.NotContains(t, rendered, "•")
	require.False(t, item.ToggleExpanded())
}

func TestThinkingAndAssistantContentHaveNoBlankSeparator(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, mode := range []thinkingViewMode{thinkingCollapsed, thinkingFullExpanded} {
		item := NewAssistantMessageItem(&sty, thinkingMessage("joined-content", "First thought\nSecond thought", "Answer text")).(*AssistantMessageItem)
		item.thinkingViewMode = mode
		lines := strings.Split(ansi.Strip(item.RawRender(80)), "\n")
		answerRow := -1
		for index, line := range lines {
			if strings.Contains(line, "Answer text") {
				answerRow = index
				break
			}
		}
		require.Positive(t, answerRow)
		require.NotEmpty(t, strings.TrimSpace(lines[answerRow-1]), "mode=%d", mode)
		if mode == thinkingCollapsed {
			require.True(t, strings.HasPrefix(strings.TrimSpace(lines[answerRow-1]), "╰"))
		} else {
			require.Contains(t, lines[answerRow-1], "Thought for")
		}
	}
}

func TestActiveSingleThoughtUsesAnimatedCollapsedLabel(t *testing.T) {
	sty := styles.CharmtonePantera()
	msg := &message.Message{
		ID:   "active-single",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "Incomplete fragment", StartedAt: testStartedAt},
		},
	}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
	step := item.StartAnimation()().(anim.StepMsg)
	for phase, want := range []string{"Thinking...", "Thinking..", "Thinking.", "Thinking..."} {
		rendered := ansi.Strip(item.cachedThinking(80))
		require.Contains(t, rendered, want)
		require.NotContains(t, rendered, "Incomplete fragment")
		if phase == 3 {
			break
		}
		for range 8 {
			next := item.Animate(step)
			require.NotNil(t, next)
			step = next().(anim.StepMsg)
		}
	}
	require.False(t, item.ToggleExpanded())
}

func TestBlankThinkingRowsAreSkipped(t *testing.T) {
	sty := styles.CharmtonePantera()
	item := NewAssistantMessageItem(&sty, thinkingMessage("blanks", "\n \t\n\n", "Answer")).(*AssistantMessageItem)
	require.Empty(t, item.cachedThinking(80))
	require.Zero(t, item.thinkingBoxHeight)
	require.False(t, item.ToggleExpanded())
	require.False(t, item.HandleMouseClick(ansi.MouseLeft, 4, 0))

	require.Nil(t, item.SetMessage(thinkingMessage("blanks", "First\n\n \t\nSecond", "Answer")))
	require.Contains(t, ansi.Strip(item.cachedThinking(80)), "THOUGHTS ▾")
	require.True(t, item.ToggleExpanded())
	rendered := ansi.Strip(item.cachedThinking(80))
	bullets := 0
	for _, line := range strings.Split(rendered, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "• ") {
			continue
		}
		bullets++
		require.NotEmpty(t, strings.TrimSpace(strings.TrimPrefix(trimmed, "• ")))
	}
	require.Equal(t, 2, bullets)
	require.Contains(t, rendered, "• First")
	require.Contains(t, rendered, "• Second")

	require.Nil(t, item.SetMessage(thinkingMessage("blanks", "\nOnly one\n \n", "Answer")))
	single := ansi.Strip(item.cachedThinking(80))
	require.Contains(t, single, "Only one")
	require.NotContains(t, single, "THOUGHTS ▾")
	require.NotContains(t, single, "•")
	require.False(t, item.ToggleExpanded())

	require.Nil(t, item.SetMessage(thinkingMessage("blanks", "\n\n", "Answer")))
	require.Empty(t, item.cachedThinking(80))
	require.Zero(t, item.thinkingBoxHeight)
}
