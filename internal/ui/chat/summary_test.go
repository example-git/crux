package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/skills"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func summaryTestItem(sty *styles.Styles, name, input, content, metadata string) ToolMessageItem {
	call := message.ToolCall{ID: "summary", Name: name, Input: input, Finished: true}
	return NewToolMessageItem(sty, "message", call, &message.ToolResult{ToolCallID: call.ID, Content: content, Metadata: metadata}, false, "/workspace")
}

func requireSummaryWidth(t *testing.T, rendered string, width int) {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		require.LessOrEqual(t, ansi.StringWidth(line), width, ansi.Strip(line))
	}
}

func TestSummaryPanelPaintsNestedStylesUniformly(t *testing.T) {
	sty := styles.CharmtonePantera()
	rows := []string{sty.Tool.SummaryTitle.Render("title") + "  " + sty.Tool.SummaryMeta.Render("kind"), "", sty.Tool.SummaryText.Render("short\nlonger description"), "日本語の表示テスト 😀 🚀", "Text remains aligned: 漢字 emoji"}
	body := renderSummaryPanel(&sty, 32, rows, "")
	for _, line := range strings.Split(body, "\n") {
		require.Equal(t, 32, ansi.StringWidth(line), "panel row width: %q", ansi.Strip(line))
	}
	buffer := uv.NewScreenBuffer(32, lipgloss.Height(body))
	uv.NewStyledString(body).Draw(&buffer, buffer.Bounds())
	want := sty.Tool.SummaryPanel.GetBackground()
	for y := 0; y < buffer.Height(); y++ {
		for x := 0; x < buffer.Width(); x++ {
			cell := buffer.CellAt(x, y)
			require.NotNil(t, cell)
			if cell.Width == 0 {
				continue
			}
			require.NotNil(t, cell.Style.Bg, "missing background at %d,%d", x, y)
			r, g, b, a := cell.Style.Bg.RGBA()
			wr, wg, wb, wa := want.RGBA()
			require.Equal(t, []uint32{wr, wg, wb, wa}, []uint32{r, g, b, a}, "background at %d,%d", x, y)
		}
	}
}

func TestSearchSummaryGroupsHighlightsAndExpandsMatches(t *testing.T) {
	sty := styles.CharmtonePantera()
	var content strings.Builder
	content.WriteString("Found 6 matches\n/workspace/docs/contract.md:\n")
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&content, "  Line %d, Char 156: Keep identity stable %sTAIL%d\n", 11+i, strings.Repeat("through every image ", 5), i)
	}
	for _, width := range []int{32, 80, 120} {
		item := summaryTestItem(&sty, tools.SearchToolName, `{"mode":"content","pattern":"identity"}`, content.String(), "")
		view := item.Render(width)
		plain := ansi.Strip(view)
		requireSummaryWidth(t, view, width)
		require.Contains(t, plain, "Search identity")
		require.Contains(t, plain, "docs/contract.md")
		require.NotContains(t, plain, "/workspace/")
		require.NotContains(t, plain, "Line 11, Char 156")
		require.Contains(t, plain, "2 more matches")
		require.Contains(t, plain, "Space")
		require.NotContains(t, plain, "TAIL5")
		buffer := uv.NewScreenBuffer(width, lipgloss.Height(view))
		uv.NewStyledString(view).Draw(&buffer, buffer.Bounds())
		expected := uv.NewScreenBuffer(8, 1)
		uv.NewStyledString(sty.Tool.SummaryMatch.Render("identity")).Draw(&expected, expected.Bounds())
		matched := false
		for y, line := range strings.Split(plain, "\n") {
			if !strings.Contains(line, "Keep identity") {
				continue
			}
			x := strings.Index(line, "identity")
			for offset := range len("identity") {
				actual := buffer.CellAt(x+offset, y).Style
				want := expected.CellAt(offset, 0).Style
				actual.Bg = want.Bg
				require.True(t, actual.Equal(&want), "match highlighting changed")
			}
			matched = true
		}
		require.True(t, matched)
		if width == 80 {
			t.Logf("Search preview:\n%s", plain)
		}
		require.True(t, item.(Expandable).ToggleExpanded())
		expanded := item.Render(width)
		requireSummaryWidth(t, expanded, width)
		joined := strings.Join(strings.Fields(ansi.Strip(expanded)), "")
		require.Contains(t, joined, "/workspace/docs/contract.md")
		for i := 0; i < 6; i++ {
			require.Contains(t, joined, fmt.Sprintf("TAIL%d", i))
			require.Contains(t, joined, fmt.Sprintf("%d:156", 11+i))
		}
		require.Contains(t, item.(*baseToolMessageItem).formatToolForCopy(), content.String())
		require.False(t, item.(Expandable).ToggleExpanded())
		require.Equal(t, view, item.Render(width))
	}
}

func TestDirectorySummaryPrioritizesTreeAndDisclosesRemainder(t *testing.T) {
	sty := styles.CharmtonePantera()
	content := "The directory tree is shown up to a depth of 1. Use a higher depth and a specific path to see more levels.\n\n- /workspace/.ai-cli/\n  - agents/\n  - agent-memory/\n"
	for i := 0; i < 9; i++ {
		content += fmt.Sprintf("  - instructions-%d.md\n", i)
	}
	for _, width := range []int{32, 80} {
		item := summaryTestItem(&sty, tools.LSToolName, `{"path":"/workspace/.ai-cli"}`, content, "")
		plain := ansi.Strip(item.Render(width))
		requireSummaryWidth(t, plain, width)
		require.Contains(t, plain, "11 entries · depth 1")
		require.Contains(t, plain, "├─ agents/")
		require.NotContains(t, plain, "Use a higher depth")
		require.NotContains(t, plain, "- /workspace/.ai-cli/")
		require.Contains(t, plain, "3 more entries")
		require.NotContains(t, plain, "instructions-8.md")
		if width == 80 {
			t.Logf("Directory preview:\n%s", plain)
		}
		item.(Expandable).ToggleExpanded()
		expanded := ansi.Strip(item.Render(width))
		requireSummaryWidth(t, expanded, width)
		require.Contains(t, expanded, "└─ instructions-8.md")
		require.Contains(t, strings.Join(strings.Fields(expanded), ""), "/workspace/.ai-cli/")
	}
}

func TestTaskSummaryUsesFlatStatusesAndWrappedPreviews(t *testing.T) {
	sty := styles.CharmtonePantera()
	var tasks []map[string]string
	for i := 0; i < 15; i++ {
		status := "running"
		if i%2 == 0 {
			status = "failed"
		}
		tasks = append(tasks, map[string]string{"id": fmt.Sprintf("i%08d", i), "type": "image", "status": status, "description": "edit image: Keep the reference identity " + strings.Repeat("consistent across every output ", 5) + fmt.Sprintf("FINAL%d", i)})
	}
	data, err := json.Marshal(tasks)
	require.NoError(t, err)
	for _, width := range []int{32, 80, 120} {
		item := summaryTestItem(&sty, tools.TaskListToolName, `{}`, string(data), "")
		plain := ansi.Strip(item.Render(width))
		requireSummaryWidth(t, plain, width)
		require.Contains(t, plain, "15 tasks")
		require.Contains(t, plain, "failed")
		require.Contains(t, plain, "running")
		require.Contains(t, plain, "image")
		require.Contains(t, plain, "9 more tasks")
		if width == 32 {
			require.NotContains(t, plain, "FINAL0")
		}
		lines := strings.Split(plain, "\n")
		for i, line := range lines {
			if strings.Contains(line, "edit image:") {
				require.NotEmpty(t, strings.TrimSpace(lines[i+1]))
				for j := i + 1; j < len(lines) && strings.TrimSpace(lines[j]) != "" && !strings.Contains(lines[j], "more tasks"); j++ {
					require.Less(t, j-i, 3)
				}
			}
		}
		if width == 80 {
			t.Logf("Task preview:\n%s", plain)
		}
		item.(Expandable).ToggleExpanded()
		expanded := item.Render(width)
		requireSummaryWidth(t, expanded, width)
		joined := strings.Join(strings.Fields(ansi.Strip(expanded)), "")
		for i := 0; i < 15; i++ {
			require.Contains(t, joined, fmt.Sprintf("FINAL%d", i))
		}
	}
}

func TestSkillSummarySeparatesMetadataFromInstructions(t *testing.T) {
	sty := styles.CharmtonePantera()
	content := "---\nname: imagegen\ndescription: Generate and edit raster images from text and references. Preserve the requested subject and output format.\nlicense: MIT\n---\n\n# Workflow\n\nUse **references** for edits.\n\n" + strings.Repeat("Keep all instructions available.\n", 20) + "FINALINSTRUCTION"
	metadata, err := json.Marshal(skills.SkillReadResult{Name: "imagegen", Source: skills.SourceSystem, Builtin: true})
	require.NoError(t, err)
	for _, width := range []int{32, 80, 120} {
		item := summaryTestItem(&sty, tools.SkillLoadToolName, `{"name":"imagegen"}`, content, string(metadata))
		plain := ansi.Strip(item.Render(width))
		requireSummaryWidth(t, plain, width)
		require.Contains(t, plain, "Load Skill imagegen")
		require.Contains(t, plain, "built-in skill")
		require.NotContains(t, plain, `{"name"`)
		require.NotContains(t, plain, "---")
		require.NotContains(t, plain, "description:")
		require.NotContains(t, plain, "FINALINSTRUCTION")
		require.Contains(t, plain, "instruction lines")
		if width == 80 {
			t.Logf("Skill preview:\n%s", plain)
		}
		item.(Expandable).ToggleExpanded()
		expanded := ansi.Strip(item.Render(width))
		requireSummaryWidth(t, expanded, width)
		require.Contains(t, strings.Join(strings.Fields(expanded), ""), "FINALINSTRUCTION")
		require.Contains(t, expanded, "License: MIT")
		require.NotContains(t, expanded, "**references**")
		require.NotContains(t, expanded, "# Workflow")
		require.Contains(t, item.(*baseToolMessageItem).formatToolForCopy(), content)
	}
}

func TestSearchFileSummaryKeepsShortPathsAndLongPathSuffixes(t *testing.T) {
	sty := styles.CharmtonePantera()
	longPath := "/workspace/" + strings.Repeat("long-directory/", 8) + "final.txt"
	content := "/workspace/short.txt\n" + longPath
	item := summaryTestItem(&sty, tools.SearchToolName, `{"mode":"files","pattern":"*.txt"}`, content, "")
	view := ansi.Strip(item.Render(32))
	requireSummaryWidth(t, view, 32)
	require.Contains(t, view, "short.txt")
	require.Contains(t, view, "final.txt")
	require.Contains(t, view, "…")
	item.(Expandable).ToggleExpanded()
	view = ansi.Strip(item.Render(32))
	requireSummaryWidth(t, view, 32)
	require.Contains(t, strings.Join(strings.Fields(view), ""), longPath)
}

func TestSummaryFallbacksAndWarningsPreserveOutput(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, fixture := range []struct{ name, input, content, metadata string }{
		{tools.SearchToolName, `{"mode":"content","pattern":"x"}`, "Found 1 matches\n/workspace/sample:\n  binary match marker", ""},
		{tools.LSToolName, `{}`, "unexpected directory response marker", ""},
		{tools.TaskListToolName, `{}`, "unexpected task response marker", ""},
		{tools.SkillLoadToolName, `{"name":"sample"}`, "# Instructions\n\nUnparsed skill marker", ""},
		{tools.LSToolName, `{}`, "- /workspace/\n  - marker.txt\n", `{"truncated":true}`},
		{tools.SearchToolName, `{"mode":"files","pattern":"*"}`, "/workspace/marker.txt\n\n(Results are truncated. Consider using a more specific path or pattern.)", ""},
	} {
		item := summaryTestItem(&sty, fixture.name, fixture.input, fixture.content, fixture.metadata)
		item.(Expandable).ToggleExpanded()
		view := ansi.Strip(item.Render(48))
		requireSummaryWidth(t, view, 48)
		require.Contains(t, view, "marker")
		if fixture.metadata != "" {
			require.Contains(t, view, "Listing limited")
		}
		if strings.Contains(fixture.content, "Results are truncated") {
			require.Contains(t, view, "Results are truncated")
		}
	}
}

func TestSummaryCardsPreserveUnicodeAndToolErrors(t *testing.T) {
	sty := styles.CharmtonePantera()
	text := strings.Repeat("界", 40)
	fixtures := []struct{ name, input, content string }{
		{tools.SearchToolName, `{"mode":"content","pattern":"界"}`, "Found 1 matches\n/workspace/例.txt:\n  Line 11, Char 3: " + text},
		{tools.LSToolName, `{}`, "- /workspace/\n  - final/\n    - " + text + ".txt\n"},
		{tools.TaskListToolName, `{}`, `[{"id":"a12345678","type":"agent","status":"running","description":"` + text + `"}]`},
		{tools.SkillLoadToolName, `{"name":"example"}`, "---\nname: example\ndescription: Test Unicode rendering\n---\n\n" + text},
	}
	for _, fixture := range fixtures {
		for _, width := range []int{20, 32, 80} {
			item := summaryTestItem(&sty, fixture.name, fixture.input, fixture.content, "")
			requireSummaryWidth(t, item.Render(width), width)
			item.(Expandable).ToggleExpanded()
			view := ansi.Strip(item.Render(width))
			requireSummaryWidth(t, view, width)
			wantCount := 40
			if fixture.name == tools.SearchToolName {
				wantCount++
			}
			require.Equal(t, wantCount, strings.Count(view, "界"), view)
			if fixture.name == tools.LSToolName {
				require.Contains(t, view, "└─ final/")
			}
			item.SetResult(&message.ToolResult{ToolCallID: item.ID(), IsError: true, Content: "Cannot read resource\n" + strings.Repeat("failure detail ", 12) + "FINALERROR"})
			view = ansi.Strip(item.Render(width))
			requireSummaryWidth(t, view, width)
			require.Contains(t, strings.Join(strings.Fields(view), ""), "FINALERROR")
			require.NotContains(t, view, "Loaded")
		}
	}
}

func TestThinkingTextUsesCompletedMutedColor(t *testing.T) {
	sty := styles.CharmtonePantera()
	want := lipgloss.Color(*sty.ThinkingMarkdown.Document.Color)
	for _, width := range []int{40, 80} {
		for _, answer := range []string{"", "Answer"} {
			for _, startsTurn := range []bool{false, true} {
				item := NewAssistantMessageItem(&sty, thinkingMessage("muted", "THOUGHT\n\nTHOUGHT", answer)).(*AssistantMessageItem)
				item.SetThinkingStartsTurn(startsTurn)
				item.thinkingViewMode = thinkingFullExpanded
				out := item.cachedThinking(width)
				buffer := uv.NewScreenBuffer(width, lipgloss.Height(out))
				uv.NewStyledString(out).Draw(&buffer, buffer.Bounds())
				found := 0
				for y, line := range strings.Split(ansi.Strip(out), "\n") {
					index := strings.Index(line, "THOUGHT")
					if index < 0 {
						continue
					}
					found++
					cell := buffer.CellAt(ansi.StringWidth(line[:index]), y)
					require.NotNil(t, cell.Style.Fg)
					r, g, b, a := cell.Style.Fg.RGBA()
					wr, wg, wb, wa := want.RGBA()
					require.Equal(t, []uint32{wr, wg, wb, wa}, []uint32{r, g, b, a}, "answer=%q startsTurn=%v", answer, startsTurn)
				}
				require.Equal(t, 2, found)
			}
		}
	}
}

func TestTodoStatusStaysOneLine(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, metadata := range []string{
		`{"is_new":true,"total":2,"just_started":"PRIVATE TASK"}`,
		`{"total":2,"completed":1,"just_completed":["PRIVATE TASK"],"just_started":"NEXT TASK"}`,
		`{"total":2,"completed":2,"just_completed":["PRIVATE TASK"]}`,
		``, `{invalid`,
	} {
		for _, width := range []int{40, 80, 160} {
			item := summaryTestItem(&sty, tools.TodosToolName, `{"todos":[{"content":"PRIVATE TASK","status":"in_progress"}]}`, "Updated", metadata)
			before := item.Render(width)
			require.Equal(t, 1, lipgloss.Height(before))
			requireSummaryWidth(t, before, width)
			require.Contains(t, ansi.Strip(before), "To-Do")
			require.NotContains(t, ansi.Strip(before), "PRIVATE TASK")
			require.NotContains(t, ansi.Strip(before), "Task details")
			require.False(t, item.(Expandable).ToggleExpanded())
			require.Equal(t, before, item.Render(width))
		}
	}
}

func TestTodoErrorsRemainExpandable(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, width := range []int{40, 80, 160} {
		item := summaryTestItem(&sty, tools.TodosToolName, `{"todos":[]}`, "Updated", "")
		item.SetResult(&message.ToolResult{ToolCallID: item.ID(), IsError: true, Content: "Cannot update tasks\n" + strings.Repeat("Error detail\n", 30) + "FINALERROR"})
		collapsed := ansi.Strip(item.Render(width))
		require.Contains(t, collapsed, "Cannot update tasks")
		require.NotContains(t, collapsed, "FINALERROR")
		require.True(t, item.(Expandable).ToggleExpanded())
		expanded := item.Render(width)
		require.Contains(t, ansi.Strip(expanded), "FINALERROR")
		requireSummaryWidth(t, expanded, width)
		item.SetResult(&message.ToolResult{ToolCallID: item.ID(), Content: "Updated"})
		require.Equal(t, 1, lipgloss.Height(item.Render(width)))
		require.False(t, item.(Expandable).ToggleExpanded())
	}
}

func TestThinkingAndTodoPresentationStaysInsetAndBounded(t *testing.T) {
	sty := styles.CharmtonePantera()
	msg := thinkingMessage("thinking-summary", "Loading the skill\n\nPlanning the inspection\n\nReading definitions", "Done")
	item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
	item.RawRender(48)
	view := ansi.Strip(item.thinkingSec.out)
	requireSummaryWidth(t, view, 48)
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		if strings.Contains(line, "Thought for") {
			require.NotEmpty(t, strings.TrimSpace(lines[i-1]))
		}
	}
	require.NotEqual(t, sty.Tool.SummaryPanel.GetBackground(), sty.Messages.ThinkingBox.GetBackground())
	require.Equal(t, 2, sty.Messages.ThinkingBox.GetHorizontalFrameSize())
	t.Logf("Thinking preview:\n%s", view)

	todos := []session.Todo{{Content: strings.Repeat("Inspect task details ", 10) + "FINALTODO", Status: session.TodoStatusInProgress}}
	params, err := json.Marshal(tools.TodosParams{Todos: []tools.TodoItem{{Content: todos[0].Content, Status: "in_progress"}}})
	require.NoError(t, err)
	metadata, err := json.Marshal(tools.TodosResponseMetadata{IsNew: true, Total: 1, Todos: todos})
	require.NoError(t, err)
	todo := summaryTestItem(&sty, tools.TodosToolName, string(params), "Updated", string(metadata))
	requireSummaryWidth(t, todo.Render(48), 48)
	todo.(Expandable).ToggleExpanded()
	view = ansi.Strip(todo.Render(48))
	requireSummaryWidth(t, view, 48)
	require.NotContains(t, view, "FINALTODO")
	require.NotContains(t, view, "Task details")
	require.Equal(t, 1, lipgloss.Height(view))
}
