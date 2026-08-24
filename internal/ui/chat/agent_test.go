package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestAgentToolTypeLabel(t *testing.T) {
	require.Equal(t, "Task", agentTypeLabel(`{"prompt":"search"}`))
	require.Equal(t, "reviewer", agentTypeLabel(`{"prompt":"review","subagent_type":"reviewer"}`))
	require.Equal(t, "Task", agentTypeLabel(`{malformed`))
	require.Equal(t, "Task", agentTypeLabel(""))
}

func TestAgentToolRendererShowsCustomTypeInExpandedAndDefaultStates(t *testing.T) {
	sty := styles.CharmtonePantera()
	toolCall := message.ToolCall{
		ID:       "agent-custom",
		Name:     "agent",
		Input:    `{"prompt":"Review this change\nwith details","subagent_type":"reviewer"}`,
		Finished: true,
	}
	result := &message.ToolResult{ToolCallID: toolCall.ID, Content: "Review complete"}
	item := NewAgentToolMessageItem(&sty, toolCall, result, false)

	view := ansi.Strip(item.Render(100))
	require.Contains(t, view, "reviewer")
	require.Contains(t, view, "Review this change with details")

	item.ToggleExpanded()
	expanded := ansi.Strip(item.Render(100))
	require.Contains(t, expanded, "reviewer")
	require.Contains(t, expanded, "Review this change")
	require.Contains(t, expanded, "with details")
	require.NotContains(t, expanded, "Review this change with details")

	item.SetCompact(true)
	compact := ansi.Strip(item.Render(100))
	require.Contains(t, compact, "Agent")
	require.NotContains(t, compact, "Review this change")
}

func TestAgentToolRendererKeepsGenericAndMalformedCallsCompatible(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, input := range []string{`{"prompt":"Search"}`, `{malformed`} {
		toolCall := message.ToolCall{ID: input, Name: "agent", Input: input, Finished: true}
		result := &message.ToolResult{ToolCallID: toolCall.ID, Content: "Done"}
		item := NewAgentToolMessageItem(&sty, toolCall, result, false)
		require.Contains(t, ansi.Strip(item.Render(80)), "Task")
	}
}
