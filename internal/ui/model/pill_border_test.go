package model

import (
	"image/color"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

// roundedBorderRunes are chars that only appear when a pill has a visible
// rounded border.
const roundedBorderRunes = "╭╮╰╯"

func TestVisibleTodosRollWithProgress(t *testing.T) {
	for _, active := range []int{0, 1, 4, 7, 3, 0} {
		todos := make([]session.Todo, 8)
		for i := range todos {
			todos[i] = session.Todo{Content: "task-" + string(rune('A'+i)), Status: session.TodoStatusPending}
			if i < active {
				todos[i].Status = session.TodoStatusCompleted
			} else if i == active {
				todos[i].Status = session.TodoStatusInProgress
			}
		}
		visible := visibleTodos(todos)
		require.Len(t, visible, 4)
		start := max(0, min(active-1, 4))
		require.Equal(t, todos[start:start+4], visible)
		u := newTestUI()
		u.session = &session.Session{ID: "s1", Todos: todos}
		u.pillsExpanded = true
		u.updateLayoutAndSize()
		u.renderPills()
		require.Equal(t, 7, u.pillsAreaHeight())
		require.Len(t, strings.Split(u.pillsView, "\n"), 7)
		for i, todo := range todos {
			if i >= start && i < start+4 {
				require.Contains(t, ansi.Strip(u.pillsView), todo.Content)
			} else {
				require.NotContains(t, ansi.Strip(u.pillsView), todo.Content)
			}
		}
	}
	require.Empty(t, visibleTodos(nil))
}

func TestExpandedTodoFence(t *testing.T) {
	for _, width := range []int{45, 65, 120, 160} {
		p, err := NewPreview()
		require.NoError(t, err)
		_, err = p.Render(PreviewOptions{Cols: width, Rows: 45, Model: "dummy-coder", Example: "tool-todos", Scenario: "working", Compact: true})
		require.NoError(t, err)
		m := p.ui
		m.session.Todos = []session.Todo{
			{Content: strings.Repeat("long goal ", 30), Status: session.TodoStatusPending},
			{Content: "original goal", ActiveForm: "active\nmultiline goal", Status: session.TodoStatusInProgress},
		}
		original := append([]session.Todo(nil), m.session.Todos...)
		m.pillsExpanded = false
		m.updateLayoutAndSize()
		collapsed := m.pillsView
		m.togglePillsExpanded()
		screen := uv.NewScreenBuffer(width, 45)
		m.Draw(screen, screen.Bounds())
		area := m.layout.pills
		require.Equal(t, m.layout.editor.Min.Y, area.Max.Y)
		area.Max.X = area.Min.X + ansi.StringWidth(strings.Split(m.pillsView, "\n")[0])
		require.LessOrEqual(t, area.Dx(), 86)
		require.Equal(t, "╭", screen.CellAt(area.Min.X, area.Min.Y).Content)
		require.Equal(t, "╮", screen.CellAt(area.Max.X-1, area.Min.Y).Content)
		require.Equal(t, len(original)+3, area.Dy())
		require.Len(t, strings.Split(m.pillsView, "\n"), area.Dy())
		require.Contains(t, ansi.Strip(m.pillsView), "ctrl+t close")
		require.Contains(t, ansi.Strip(m.pillsView), "…")
		for y := area.Min.Y; y < area.Max.Y; y++ {
			for x := area.Min.X; x < area.Max.X; x++ {
				cell := screen.CellAt(x, y)
				require.NotNil(t, cell.Style.Bg, "width=%d x=%d y=%d content=%q", width, x, y, cell.Content)
				require.Equal(t, color.RGBAModel.Convert(m.com.Styles.Background), color.RGBAModel.Convert(cell.Style.Bg), "width=%d x=%d y=%d", width, x, y)
				if y == area.Min.Y {
					require.Equal(t, color.RGBAModel.Convert(m.editorAccent()), color.RGBAModel.Convert(cell.Style.Fg))
				}
			}
		}
		require.Equal(t, "╰", screen.CellAt(area.Min.X, area.Max.Y-2).Content)
		require.Equal(t, "╯", screen.CellAt(area.Max.X-1, area.Max.Y-2).Content)
		require.Empty(t, strings.TrimSpace(ansi.Strip(strings.Split(m.pillsView, "\n")[area.Dy()-1])))
		require.Equal(t, original, m.session.Todos)
		m.togglePillsExpanded()
		require.Equal(t, collapsed, m.pillsView)
	}
}

func TestExpandedTodoBoxFitsGoalsAndFloatsAboveInput(t *testing.T) {
	for _, width := range []int{65, 120, 160} {
		for _, compact := range []bool{false, true} {
			for _, queued := range []int{0, 2} {
				p, err := NewPreview()
				require.NoError(t, err)
				_, err = p.Render(PreviewOptions{Cols: width, Rows: 45, Model: "dummy-coder", Example: "tool-todos", Scenario: "working", Compact: compact, Input: "preserved draft"})
				require.NoError(t, err)
				m := p.ui
				m.session.Todos = []session.Todo{{Content: "Inspect the input attachment", Status: session.TodoStatusPending}}
				m.promptQueue = queued
				m.pillsExpanded = true
				m.focusedPillSection = pillSectionTodos
				m.updateLayoutAndSize()
				screen := uv.NewScreenBuffer(width, 45)
				m.Draw(screen, screen.Bounds())
				rows := strings.Split(m.pillsView, "\n")
				boxWidth := ansi.StringWidth(rows[len(rows)-1])
				require.Equal(t, min(m.layout.pills.Dx(), len("Inspect the input attachment")+6), boxWidth)
				require.Equal(t, m.layout.editor.Min.Y, m.layout.pills.Max.Y)
				y := m.layout.editor.Min.Y - 2
				require.Equal(t, "╰", screen.CellAt(m.layout.pills.Min.X, y).Content)
				require.Equal(t, "╯", screen.CellAt(m.layout.pills.Min.X+boxWidth-1, y).Content)
				for x := m.layout.pills.Min.X; x < m.layout.pills.Min.X+boxWidth; x++ {
					cell := screen.CellAt(x, y+1)
					require.Equal(t, " ", cell.Content)
					require.Equal(t, color.RGBAModel.Convert(m.com.Styles.Background), color.RGBAModel.Convert(cell.Style.Bg))
				}
				if boxWidth < m.layout.pills.Dx() {
					cell := screen.CellAt(m.layout.pills.Min.X+boxWidth, y)
					if cell.Style.Bg != nil {
						require.Equal(t, color.RGBAModel.Convert(m.com.Styles.Background), color.RGBAModel.Convert(cell.Style.Bg))
					}
				}
				require.Equal(t, "preserved draft", m.textarea.Value())
			}
		}
	}
}

func hasRoundedBorder(s string) bool {
	return strings.ContainsAny(s, roundedBorderRunes)
}

// queuePillHasBorder reports whether the "N Queued" pill is wrapped in a
// rounded border by checking the line directly above the queue label for a
// top border corner.
func queuePillHasBorder(view string) bool {
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "Queued") {
			continue
		}
		if i == 0 {
			return false
		}
		return strings.ContainsAny(lines[i-1], "╭╮")
	}
	return false
}

// TestQueuePillAlwaysHasBorder verifies that the queued-prompts pill renders
// with its rounded border regardless of panel expansion or which pill
// section is nominally focused.
func TestQueuePillAlwaysHasBorder(t *testing.T) {
	incompleteTodos := []session.Todo{{Content: "a", Status: session.TodoStatusPending}}

	cases := []struct {
		name           string
		expanded       bool
		focusedSection pillSection
		todos          []session.Todo
		queue          int
	}{
		{"collapsed only queue", false, pillSectionTodos, nil, 2},
		{"collapsed queue+todos", false, pillSectionTodos, incompleteTodos, 2},
		{"expanded queue focused", true, pillSectionQueue, nil, 2},
		{"expanded stale todos focus only queue", true, pillSectionTodos, nil, 2},
		{"expanded todos focused queue+todos", true, pillSectionTodos, incompleteTodos, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newTestUI()
			u.session = &session.Session{ID: "s1", Todos: tc.todos}
			u.promptQueue = tc.queue
			u.pillsExpanded = tc.expanded
			u.focusedPillSection = tc.focusedSection
			u.updateLayoutAndSize()
			u.renderPills()

			if !hasRoundedBorder(u.pillsView) {
				t.Fatalf("expected a rounded border somewhere in pills view:\n%s", u.pillsView)
			}
			if !queuePillHasBorder(u.pillsView) {
				t.Fatalf("expected the queue pill to have a border:\n%s", u.pillsView)
			}
		})
	}
}

// TestEffectiveFocusedSectionFallsThrough verifies that a stale focused section
// (pointing at a section with no content) resolves to the section that still
// has content, so the expanded list stays populated.
func TestEffectiveFocusedSectionFallsThrough(t *testing.T) {
	cases := []struct {
		name     string
		stored   pillSection
		todos    []session.Todo
		queue    int
		expected pillSection
	}{
		{"todos focus but only queue", pillSectionTodos, nil, 2, pillSectionQueue},
		{"queue focus but only todos", pillSectionQueue, []session.Todo{{Content: "a", Status: session.TodoStatusPending}}, 0, pillSectionTodos},
		{"todos focus with todos", pillSectionTodos, []session.Todo{{Content: "a", Status: session.TodoStatusPending}}, 2, pillSectionTodos},
		{"queue focus with queue", pillSectionQueue, nil, 2, pillSectionQueue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newTestUI()
			u.session = &session.Session{ID: "s1", Todos: tc.todos}
			u.promptQueue = tc.queue
			u.focusedPillSection = tc.stored
			if got := u.effectiveFocusedSection(); got != tc.expected {
				t.Fatalf("effectiveFocusedSection() = %d, want %d", got, tc.expected)
			}
		})
	}
}
