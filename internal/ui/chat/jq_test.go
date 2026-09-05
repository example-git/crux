package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestJQRendererShowsFilterSourceAndOutput(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{
		ID:       "jq-call",
		Name:     tools.JQToolName,
		Input:    `{"filter":".items[] | .name","files":["data.json"]}`,
		Finished: true,
	}
	result := &message.ToolResult{ToolCallID: call.ID, Content: "one\ntwo\n"}
	view := ansi.Strip(NewToolMessageItem(&sty, "message", call, result, false, "").Render(100))

	require.Contains(t, view, "jq")
	require.Contains(t, view, ".items[] | .name")
	require.Contains(t, view, "data.json")
	require.Contains(t, view, "one")
	require.Contains(t, view, "two")
	require.NotContains(t, view, "TODO")
}

func TestJQRendererUsesDefaultFilterAndCompactMode(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{ID: "jq-call", Name: tools.JQToolName, Input: `{}`, Finished: true}
	result := &message.ToolResult{ToolCallID: call.ID, Content: "result"}
	item := NewToolMessageItem(&sty, "message", call, result, false, "")
	item.(Compactable).SetCompact(true)
	view := ansi.Strip(item.Render(80))

	require.Contains(t, view, "jq")
	require.Contains(t, view, ".")
	require.NotContains(t, view, "result")
	require.Equal(t, 1, len(strings.Split(view, "\n")))
}

func TestJQRendererHandlesInvalidInput(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{ID: "jq-call", Name: tools.JQToolName, Input: `{malformed`, Finished: true}
	result := &message.ToolResult{ToolCallID: call.ID, Content: "result"}
	view := ansi.Strip(NewToolMessageItem(&sty, "message", call, result, false, "").Render(80))
	require.Contains(t, view, "Invalid parameters")
}
