package chat

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestAssistantMessageItemExpandable guards the Expandable contract on
// AssistantMessageItem along the keyboard-driven expand path. The earlier
// implementation returned no value, which meant the type silently did
// not satisfy chat.Expandable and the keyboard-driven expand path in
// model/chat.go skipped thinking blocks.
//
// We exercise the contract through the bare Expandable interface (the
// same dispatch site model.Chat.ToggleExpandedSelectedItem uses), which
// proves both that AssistantMessageItem still satisfies the interface
// and that the bool return reports the right semantic state at every
// point in the cycle.
func TestAssistantMessageItemExpandable(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	// Short thinking: under the tail-window cap, so the cycle is
	// collapsed -> full -> collapsed (tail-window is skipped).
	msg := thinkingMessage("m1", "step one\nstep two\nstep three", "")
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)

	exp, ok := any(item).(Expandable)
	require.True(t, ok, "AssistantMessageItem must satisfy Expandable")

	require.Equal(t, thinkingCollapsed, item.thinkingViewMode,
		"new items must start in the collapsed view-mode")
	require.True(t, exp.ToggleExpanded(),
		"first toggle of a non-empty thinking block must report expanded")
	require.Equal(t, thinkingFullExpanded, item.thinkingViewMode,
		"short blocks must skip tail-window and land in full expansion")
	require.False(t, exp.ToggleExpanded(),
		"second toggle must report collapsed (cycle closed)")
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode)
}

// TestAssistantMessageItemExpandableEmptyThinkingNoOp guards the B2
// fix: a message with no thinking text must treat ToggleExpanded as a
// no-op. Mutating the view mode in that case would thrash the
// thinking-section cache key for no visible benefit and would surprise
// the caller (model.Chat.ToggleExpandedSelectedItem would treat a
// "now collapsed" return as a real state change and re-scroll on it).
func TestAssistantMessageItemExpandableEmptyThinkingNoOp(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	msg := &message.Message{ID: "m1-empty", Role: message.Assistant}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)

	exp, ok := any(item).(Expandable)
	require.True(t, ok, "AssistantMessageItem must satisfy Expandable")

	require.Equal(t, thinkingCollapsed, item.thinkingViewMode)
	require.False(t, exp.ToggleExpanded(),
		"empty thinking must report current (collapsed) state without flipping")
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode,
		"empty-thinking toggle must not mutate thinkingViewMode")

	// Whitespace-only thinking is still effectively empty.
	item.message.Parts = []message.ContentPart{
		message.ReasoningContent{Thinking: "  \n\n\t  ", StartedAt: testStartedAt},
	}
	require.False(t, exp.ToggleExpanded())
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode)
}

// TestAssistantMessageItemTailWindowBoundary guards the B1 fix: the
// tail-window heuristic must compare logical line counts (1 +
// newline count) against the cap, not raw newline counts. A source
// whose logical line count exactly equals the cap must NOT trip the
// tail-window step (full render still fits cleanly under the cap),
// while one logical line over the cap must trip it.
func TestAssistantMessageItemRendersRetryStatusInRed(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	msg := &message.Message{ID: "retry", Role: message.Assistant}
	msg.SetRetrying()
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)

	rendered := item.RawRender(100)
	require.Contains(t, ansi.Strip(rendered), "Thinking")
	redT := lipgloss.NewStyle().Foreground(sty.RetryLabelColor).Render("T")
	require.Contains(t, rendered, redT)

	msg.AppendReasoningContent("Trying again")
	item.SetMessage(msg)
	rendered = item.RawRender(100)
	require.Contains(t, ansi.Strip(rendered), "Thinking")
	providerT := lipgloss.NewStyle().Foreground(sty.WorkingLabelColor).Render("T")
	require.Contains(t, rendered, providerT)
}

func TestAssistantSummaryMessageIsDistinctAndCollapsible(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	msg := &message.Message{
		ID:               "summary",
		Role:             message.Assistant,
		IsSummaryMessage: true,
		Parts: []message.ContentPart{
			message.TextContent{Text: "# Preview heading\n\nPreview body\n\n## Hidden heading\n\nHidden body"},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: testFinishTime},
		},
	}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)

	collapsed := item.Render(100)
	collapsedPlain := ansi.Strip(collapsed)
	require.Contains(t, collapsed, sty.Messages.SummaryHeader.Render("▸ Conversation Summary"))
	require.Contains(t, collapsedPlain, "click or space to expand")
	require.Contains(t, collapsedPlain, "Preview heading")
	require.Contains(t, collapsedPlain, "Preview body")
	require.NotContains(t, collapsedPlain, "Hidden body")
	require.True(t, item.HandleMouseClick(ansi.MouseLeft, 0, 0))
	require.False(t, item.HandleMouseClick(ansi.MouseLeft, 0, 1))
	require.False(t, item.HandleMouseClick(ansi.MouseRight, 0, 0))

	require.True(t, item.ToggleExpanded())
	expandedPlain := ansi.Strip(item.Render(100))
	require.Contains(t, expandedPlain, "▾ Conversation Summary")
	require.Contains(t, expandedPlain, "click or space to collapse")
	require.Contains(t, expandedPlain, "Hidden body")

	require.False(t, item.ToggleExpanded())
	recollapsedPlain := ansi.Strip(item.Render(100))
	require.Contains(t, recollapsedPlain, "Preview body")
	require.NotContains(t, recollapsedPlain, "Hidden body")
}

func TestAssistantSummaryPlaceholderTransitionsFromSpinnerToPreview(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	pending := &message.Message{
		ID:               "summary-placeholder",
		Role:             message.Assistant,
		IsSummaryMessage: true,
	}
	item := NewAssistantMessageItem(&sty, pending).(*AssistantMessageItem)

	require.NotNil(t, item.StartAnimation())
	pendingView := item.Render(100)
	pendingPlain := ansi.Strip(pendingView)
	require.Contains(t, pendingView, sty.Messages.SummaryHeader.Render("▸ Conversation Summary"))
	require.Contains(t, pendingPlain, "Summarizing")
	require.NotContains(t, pendingPlain, "click or space to expand")

	finished := pending.Clone()
	finished.Parts = []message.ContentPart{
		message.TextContent{Text: "# Completed preview\n\nVisible context\n\nHidden context"},
		message.Finish{Reason: message.FinishReasonEndTurn, Time: testFinishTime},
	}
	require.Nil(t, item.SetMessage(&finished))
	finishedPlain := ansi.Strip(item.Render(100))
	require.NotContains(t, finishedPlain, "Summarizing")
	require.Contains(t, finishedPlain, "Completed preview")
	require.NotContains(t, finishedPlain, "Hidden context")
}

func TestAssistantMessageItemTailWindowBoundary(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()

	atCap := buildLines(maxExpandedThinkingTailLines)
	overCap := buildLines(maxExpandedThinkingTailLines + 1)

	atItem := NewAssistantMessageItem(&sty, thinkingMessage("at-cap", atCap, "")).(*AssistantMessageItem)
	require.False(t, atItem.tailWindowWouldTruncate(),
		"a source with exactly N logical lines must not trip the tail-window step")

	overItem := NewAssistantMessageItem(&sty, thinkingMessage("over-cap", overCap, "")).(*AssistantMessageItem)
	require.True(t, overItem.tailWindowWouldTruncate(),
		"a source with N+1 logical lines must trip the tail-window step")
}

// buildLines returns a string of n logical lines (n-1 newlines). Each
// line is a unique short token so callers can distinguish head from
// tail in rendered output if they need to.
func buildLines(n int) string {
	if n <= 0 {
		return ""
	}
	var b []byte
	for i := 1; i <= n; i++ {
		if i > 1 {
			b = append(b, '\n')
		}
		b = append(b, 'l', 'n')
		b = append(b, []byte(itoa(i))...)
	}
	return string(b)
}

// TestAssistantMessageItemHandleMouseClick ensures HandleMouseClick does not
// toggle expansion on its own. The generic Expandable path in
// model/chat.go does the toggle; doing it here too would double-toggle and
// net to no change.
func TestAssistantMessageItemHandleMouseClick(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	msg := &message.Message{ID: "m2", Role: message.Assistant}
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
	item.thinkingBoxHeight = 5

	// Click inside the thinking box signals handled but must not mutate
	// the view-mode state.
	require.True(t, item.HandleMouseClick(ansi.MouseLeft, 0, 2))
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode,
		"HandleMouseClick must not toggle expansion on its own")

	// Click outside the thinking box is ignored entirely.
	require.False(t, item.HandleMouseClick(ansi.MouseLeft, 0, 10))
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode)

	// Non-left button is ignored.
	require.False(t, item.HandleMouseClick(ansi.MouseRight, 0, 2))
	require.Equal(t, thinkingCollapsed, item.thinkingViewMode)
}
