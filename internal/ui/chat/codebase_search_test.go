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

func TestCodebaseSearchRendererShowsStyledQueryAndMatches(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{
		ID:       "codebase-search",
		Name:     tools.CodebaseSearchToolName,
		Input:    `{"query":"session cancellation flow","count":4,"path_prefix":"internal/agent"}`,
		Finished: true,
	}
	result := &message.ToolResult{ToolCallID: call.ID, Content: strings.TrimSpace(`Found 2 semantic matches:

Direct implementation
1. internal/agent/coordinator.go:120-128 (score 0.9123, symbol coordinator.Cancel)
Matches cancellation + session.
func (c *coordinator) Cancel() {
    c.cancel()
}

Related behavior
2. internal/agent/agent.go:44-51 (score 0.8456, symbol stopRun)
Matches cancellation.
func stopRun() error {
    return nil
}`)}
	item := NewToolMessageItem(&sty, "message", call, result, false, "")

	rendered := item.Render(110)
	require.Contains(t, rendered, "\x1b[")
	view := ansi.Strip(rendered)
	require.Contains(t, view, "Codebase Search")
	require.Contains(t, view, "session cancellation flow")
	require.Contains(t, view, "path=internal/agent")
	require.Contains(t, view, "2 semantic matches")
	require.Contains(t, view, "Direct implementation")
	require.Contains(t, view, "Related behavior")
	require.Contains(t, view, "internal/agent/coordinator.go:120-128")
	require.Contains(t, view, "score 0.9123")
	require.Contains(t, view, "coordinator.Cancel")
	require.Contains(t, view, "Matches cancellation + session.")
	require.Contains(t, view, "func (c *coordinator) Cancel()")
	require.NotContains(t, view, call.Input)
}

func TestCodebaseSearchRoleBeforeRecognizesSupportingEvidenceRoles(t *testing.T) {
	for _, role := range []string{"Delivery/call path", "Recovery/retry", "Payload/construction", "Comparison", "Parallel implementation", "Persistence"} {
		require.Equal(t, role, codebaseSearchRoleBefore("Found matches\n\n"+role+"\n"))
	}
}

func TestCodebaseSearchRendererBoundsCollapsedMatches(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{ID: "codebase-search", Name: tools.CodebaseSearchToolName, Input: `{"query":"handlers"}`, Finished: true}
	result := &message.ToolResult{ToolCallID: call.ID, Content: strings.TrimSpace(`Found 4 semantic matches:

1. one.go:1-2 (score 0.9000)
one

2. two.go:2-3 (score 0.8000)
two

3. three.go:3-4 (score 0.7000)
three

4. four.go:4-5 (score 0.6000)
four`)}
	item := NewToolMessageItem(&sty, "message", call, result, false, "")

	collapsed := ansi.Strip(item.Render(100))
	require.Contains(t, collapsed, "one.go:1-2")
	require.Contains(t, collapsed, "three.go:3-4")
	require.Contains(t, collapsed, "… 1 more matches")
	require.NotContains(t, collapsed, "four.go:4-5")

	item.(Expandable).ToggleExpanded()
	expanded := ansi.Strip(item.Render(100))
	require.Contains(t, expanded, "four.go:4-5")
	require.NotContains(t, expanded, "… 1 more matches")
}

func TestCodebaseSearchRendererHandlesNoMatchesAndInvalidInput(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{ID: "none", Name: tools.CodebaseSearchToolName, Input: `{"query":"missing"}`, Finished: true}
	result := &message.ToolResult{ToolCallID: call.ID, Content: "No semantic matches found."}
	view := ansi.Strip(NewToolMessageItem(&sty, "message", call, result, false, "").Render(80))
	require.Contains(t, view, "No semantic matches found.")

	call.Input = `{malformed`
	view = ansi.Strip(NewToolMessageItem(&sty, "message", call, result, false, "").Render(80))
	require.Contains(t, view, "Invalid parameters")
}
