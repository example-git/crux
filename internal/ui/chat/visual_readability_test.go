package chat

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestExpandedToolResultsRevealLongLines(t *testing.T) {
	sty := styles.CharmtonePantera()
	content := strings.Repeat("prefix ", 35) + "ENDMARK"
	for _, name := range []string{tools.BashToolName, tools.ViewToolName, "unknown_tool", "mcp_test_result"} {
		t.Run(name, func(t *testing.T) {
			call := message.ToolCall{ID: name, Name: name, Input: `{"command":"printf text","file_path":"sample.txt","offset":40}`, Finished: true}
			item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{Content: content}, false, "")
			collapsed := ansi.Strip(item.Render(48))
			require.NotContains(t, collapsed, "ENDMARK")
			require.True(t, item.(Expandable).ToggleExpanded())
			expanded := ansi.Strip(item.Render(48))
			require.Contains(t, strings.Join(strings.Fields(expanded), ""), "ENDMARK")
			for _, line := range strings.Split(expanded, "\n") {
				require.LessOrEqual(t, ansi.StringWidth(line), 48, line)
			}
			require.False(t, item.(Expandable).ToggleExpanded())
			require.Equal(t, collapsed, ansi.Strip(item.Render(48)))
		})
	}
}

func TestExpandedCodeKeepsSourceNumbersOnFirstVisualRow(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{ID: "view", Name: tools.ViewToolName, Input: `{"file_path":"sample.txt","offset":40}`, Finished: true}
	item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{Content: strings.Repeat("界", 45) + "\nlast"}, false, "")
	item.(Expandable).ToggleExpanded()
	view := ansi.Strip(item.Render(32))
	require.Regexp(t, `(?m)^\s*41\s+界`, view)
	require.Regexp(t, `(?m)^\s*42\s+last`, view)
	require.NotRegexp(t, `(?m)^\s*43\s`, view)
	require.Equal(t, 45, strings.Count(view, "界"))
	for _, line := range strings.Split(view, "\n") {
		require.LessOrEqual(t, ansi.StringWidth(line), 32)
	}
}

func TestExpandedToolErrorsPreserveMultilineDetails(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, name := range []string{tools.BashToolName, tools.ViewToolName, tools.WriteToolName, "unknown_tool"} {
		t.Run(name, func(t *testing.T) {
			call := message.ToolCall{ID: name, Name: name, Input: `{}`, Finished: true}
			item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{IsError: true, Content: "first failure\n" + strings.Repeat("detail ", 30) + "LASTERROR\nlast failure"}, false, "")
			require.NotContains(t, ansi.Strip(item.Render(48)), "LASTERROR")
			item.(Expandable).ToggleExpanded()
			view := ansi.Strip(item.Render(48))
			require.Contains(t, strings.Join(strings.Fields(view), ""), "LASTERROR")
			require.Contains(t, view, "last failure")
			require.Regexp(t, `first failure\s*\n`, view)
		})
	}
}

func TestToolMarkdownUsesMarkdownPresentation(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, name := range []string{tools.FetchToolName, "unknown_tool", "mcp_test_result"} {
		t.Run(name, func(t *testing.T) {
			call := message.ToolCall{ID: name, Name: name, Input: `{"url":"https://example.com","format":"markdown"}`, Finished: true}
			item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{Content: "# Heading\n\nA **bold** result.\n\n- list item"}, false, "")
			item.(Expandable).ToggleExpanded()
			view := ansi.Strip(item.Render(80))
			require.Contains(t, view, "Heading")
			require.Contains(t, view, "bold")
			require.NotContains(t, view, "**bold**")
			require.NotContains(t, view, "# Heading")
		})
	}
}

func TestTaskResultsKeepDiagnosticsWithPartialOutput(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, output := range []string{"", "partial output"} {
		exitCode := 7
		data, err := json.Marshal(managedtask.OutputResult{Task: managedtask.View{ID: "b12345678", State: managedtask.State{Status: managedtask.StatusFailed, ErrorMessage: "provider failed", LostReason: "worker unavailable", ExitCode: &exitCode}}, Output: output})
		require.NoError(t, err)
		call := message.ToolCall{ID: "output", Name: tools.TaskOutputToolName, Input: `{"task_id":"b12345678"}`, Finished: true}
		item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{Content: string(data)}, false, "")
		item.(Expandable).ToggleExpanded()
		view := ansi.Strip(item.Render(80))
		for _, want := range []string{"provider failed", "worker unavailable", "Exit code: 7", output} {
			require.Contains(t, view, want)
		}
		require.NotContains(t, view, "no output")
	}
}

func TestMemoryTopicIncludesBody(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{ID: "memory", Name: tools.MemoryListToolName, Input: `{"scope":"project","topic":"example"}`, Finished: true}
	item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{Content: `{"name":"Example","description":"Summary","content":"## Body\n\nActual **memory content**."}`}, false, "")
	item.(Expandable).ToggleExpanded()
	view := ansi.Strip(item.Render(80))
	require.Contains(t, view, "Actual memory content.")
}

func TestCodebaseSnippetUsesReportedSourceLine(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{ID: "search", Name: tools.CodebaseSearchToolName, Input: `{"query":"snippet"}`, Finished: true}
	item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{Content: "1. sample.go:41-42 (score 0.9)\nfirst source line\nsecond source line"}, false, "")
	view := ansi.Strip(item.Render(80))
	require.Regexp(t, `(?m)^\s*41\s+first source line`, view)
	require.Regexp(t, `(?m)^\s*42\s+second source line`, view)
}

func TestBackgroundAgentLaunchExpansionRevealsOriginalResult(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{ID: "agent", Name: agent.AgentToolName, Input: `{"prompt":"review","run_in_background":true}`, Finished: true}
	item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{Content: "Original launch details", Metadata: `{"task_id":"a12345678","background":true}`}, false, "")
	require.NotContains(t, ansi.Strip(item.Render(80)), "Original launch details")
	item.(Expandable).ToggleExpanded()
	require.Contains(t, ansi.Strip(item.Render(80)), "Original launch details")
}

func TestExpandedEditDiffFillsWrappedRows(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, name := range []string{tools.EditToolName, tools.MultiEditToolName} {
		for _, width := range []int{160, 161} {
			metadata, err := json.Marshal(tools.MultiEditResponseMetadata{OldContent: "old", NewContent: strings.Repeat("new ", 90) + "Ω", EditsApplied: 1})
			require.NoError(t, err)
			call := message.ToolCall{ID: "edit", Name: name, Input: `{"file_path":"sample.txt"}`, Finished: true}
			item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{Content: "edited", Metadata: string(metadata)}, false, "")
			collapsed := item.Render(width)
			item.(Expandable).ToggleExpanded()
			view := item.Render(width)
			lines := strings.Split(ansi.Strip(view), "\n")
			first, last := -1, -1
			for y, line := range lines {
				if strings.Contains(line, "old") && strings.Contains(line, "new") {
					first = y
				}
				if strings.Contains(line, "Ω") {
					last = y
				}
			}
			require.GreaterOrEqual(t, first, 0)
			require.Greater(t, last, first)
			screen := uv.NewScreenBuffer(width, len(lines))
			uv.NewStyledString(view).Draw(screen, screen.Bounds())
			checked := 0
			for x := 0; x < width; x++ {
				background := screen.CellAt(x, first).Style.Bg
				if background == nil {
					continue
				}
				checked++
				for y := first + 1; y <= last; y++ {
					actual := screen.CellAt(x, y).Style.Bg
					require.NotNil(t, actual, "%s width=%d x=%d y=%d", name, width, x, y)
					require.Equal(t, color.RGBAModel.Convert(background), color.RGBAModel.Convert(actual))
				}
			}
			require.Greater(t, checked, width/2)
			item.(Expandable).ToggleExpanded()
			require.Equal(t, collapsed, item.Render(width))
		}
	}
}

func TestExpandedEditDiffRevealsLongChangedLines(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, name := range []string{tools.EditToolName, tools.MultiEditToolName} {
		for _, width := range []int{48, 160} {
			t.Run(fmt.Sprintf("%s/%d", name, width), func(t *testing.T) {
				metadata, err := json.Marshal(tools.MultiEditResponseMetadata{OldContent: strings.Repeat("old ", 80) + "OLDEND", NewContent: strings.Repeat("new ", 80) + "NEWEND", EditsApplied: 1})
				require.NoError(t, err)
				call := message.ToolCall{ID: "edit", Name: name, Input: `{"file_path":"sample.txt"}`, Finished: true}
				item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{Content: "edited", Metadata: string(metadata)}, false, "")
				require.NotContains(t, ansi.Strip(item.Render(width)), "NEWEND")
				item.(Expandable).ToggleExpanded()
				view := ansi.Strip(item.Render(width))
				require.Contains(t, strings.Join(strings.Fields(view), ""), "OLDEND")
				require.Contains(t, strings.Join(strings.Fields(view), ""), "NEWEND")
				for _, line := range strings.Split(view, "\n") {
					require.LessOrEqual(t, ansi.StringWidth(line), width)
				}
			})
		}
	}
}

func TestFetchHonorsMarkdownDefaultAndExplicitRawFormats(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, format := range []string{"", "markdown", "text", "html"} {
		t.Run(format, func(t *testing.T) {
			call := message.ToolCall{ID: "fetch", Name: tools.FetchToolName, Input: fmt.Sprintf(`{"url":"https://example.com","format":%q}`, format), Finished: true}
			item := NewToolMessageItem(&sty, "message", call, &message.ToolResult{Content: "# Heading\n\n**bold**"}, false, "")
			item.(Expandable).ToggleExpanded()
			view := ansi.Strip(item.Render(80))
			if format == "" || format == "markdown" {
				require.NotContains(t, view, "**bold**")
			} else {
				require.Contains(t, view, "**bold**")
			}
		})
	}
}
