package model

import (
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
)

func TestPreviewUsesProductionViewWithDummyModels(t *testing.T) {
	p, err := NewPreview()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range PreviewModels {
		frame, err := p.Render(PreviewOptions{Cols: 180, Rows: 75, Model: m.ID, Example: "tool-multiedit", Scenario: "working", PlanExpanded: true})
		if err != nil {
			t.Fatal(err)
		}
		plain := ansi.Strip(frame.Content)
		for _, want := range []string{m.Name, "Dummy Provider", "Modified Files", "Multi-Edit", "To-Do", "Thinking"} {
			if !strings.Contains(plain, want) {
				t.Errorf("%s: missing %q in real view:\n%s", m.ID, want, plain)
			}
		}
		if frame.Content != p.ui.View().Content {
			t.Fatal("preview must return unmodified UI.View content")
		}
	}
}
func TestPreviewTodoStatusPreservesPinnedList(t *testing.T) {
	for _, width := range []int{65, 120} {
		for _, expanded := range []bool{false, true} {
			p, err := NewPreview()
			if err != nil {
				t.Fatal(err)
			}
			frame, err := p.Render(PreviewOptions{Cols: width, Rows: 45, Model: "dummy-coder", Example: "tool-todos", Scenario: "working", PlanExpanded: true, ToolsExpanded: expanded})
			if err != nil {
				t.Fatal(err)
			}
			plain := ansi.Strip(frame.Content)
			if strings.Contains(plain, "Task details") || !strings.Contains(plain, "To-Do") {
				t.Fatalf("unexpected to-do presentation:\n%s", plain)
			}
			if len(p.ui.session.Todos) == 0 {
				t.Fatal("fixture has no pinned tasks")
			}
			textWidth := max(1, min(80, ansi.StringWidth(strings.Split(p.ui.pillsView, "\n")[0])-6))
			for _, todo := range p.ui.session.Todos {
				content := ansi.Truncate(todo.Content, textWidth, "…")
				activeForm := ansi.Truncate(todo.ActiveForm, textWidth, "…")
				if !strings.Contains(ansi.Strip(p.ui.pillsView), content) && (activeForm == "" || !strings.Contains(ansi.Strip(p.ui.pillsView), activeForm)) {
					t.Fatalf("pinned task missing: %q\n%s", todo.Content, ansi.Strip(p.ui.pillsView))
				}
			}
			if frame.Content != p.ui.View().Content {
				t.Fatal("preview must return unmodified UI.View content")
			}
		}
	}
}

func TestPreviewControlsReachRealComponents(t *testing.T) {
	p, _ := NewPreview()
	o := PreviewOptions{Cols: 180, Rows: 75, Model: "dummy-coder", Scenario: "working", PlanExpanded: true}
	base, err := p.Render(o)
	if err != nil {
		t.Fatal(err)
	}
	o.Compact = true
	o.ToolsCompact = true
	o.Input = "A local preview input"
	compact, err := p.Render(o)
	if err != nil {
		t.Fatal(err)
	}
	if !compact.Compact || compact.SidebarColumns != 0 {
		t.Fatal("compact option did not reach real layout")
	}
	if !strings.Contains(ansi.Strip(compact.Content), o.Input) {
		t.Fatal("input missing from real textarea")
	}
	if compact.Content == base.Content {
		t.Fatal("controls did not change frame")
	}
	o.Compact = false
	o.ToolsCompact = false
	o.Scenario = "error"
	o.Example = "status-error"
	o.Scroll = 0
	failure, err := p.Render(o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ansi.Strip(failure.Content), "Synthetic") {
		t.Fatal("error result missing from real tool renderer")
	}
	o.Model = "no-such-model"
	if _, err := p.Render(o); err == nil {
		t.Fatal("invalid model silently accepted")
	}
}
