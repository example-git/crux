package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/chat"
)

func TestPreviewEveryExampleRenders(t *testing.T) {
	seen := map[string]bool{}
	for _, example := range PreviewExamples() {
		if seen[example.ID] {
			t.Fatalf("duplicate example %q", example.ID)
		}
		seen[example.ID] = true
		t.Run(example.ID, func(t *testing.T) {
			p, err := NewPreview()
			if err != nil {
				t.Fatal(err)
			}
			for _, cols := range []int{60, 180} {
				frame, err := p.Render(PreviewOptions{Example: example.ID, Model: "dummy-coder", Scenario: "working", Cols: cols, Rows: 75, PlanExpanded: true})
				if err != nil {
					t.Fatal(err)
				}
				if example.ID == "system" {
					if frame.MessageCount != 0 {
						t.Fatal("system context must follow production visibility")
					}
					continue
				}
				if frame.MessageCount < 1 {
					t.Fatal("example produced no real message item")
				}
				if strings.Contains(ansi.Strip(frame.Content), "Invalid parameters") {
					t.Fatalf("invalid fixture parameters: %s", frame.Content)
				}
				if frame.Content != p.ui.View().Content {
					t.Fatal("sample did not use unmodified production view")
				}
			}
		})
	}
	p, _ := NewPreview()
	if _, err := p.Render(PreviewOptions{Example: "missing", Model: "dummy-coder", Scenario: "working", Cols: 180, Rows: 75}); err == nil {
		t.Fatal("unknown examples must fail visibly")
	}
}

// Read the factory's switch cases so a new dedicated tool renderer requires an
// explicit sample. Generic, Docker MCP and ordinary MCP examples are checked below.
func TestPreviewCoversToolFactory(t *testing.T) {
	parse := func(path string) *ast.File {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	covered := map[string]bool{}
	ast.Inspect(parse("preview_examples.go"), func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			covered[s.Sel.Name] = true
		}
		return true
	})
	factory := parse("../chat/tools.go")
	for _, decl := range factory.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "NewToolMessageItem" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range c.List {
				if s, ok := expr.(*ast.SelectorExpr); ok && !covered[s.Sel.Name] {
					t.Errorf("missing fixture for %s", s.Sel.Name)
				}
			}
			return true
		})
	}
	branches := map[string]bool{}
	for _, e := range PreviewExamples() {
		if e.tool == "fixture_custom_tool" {
			branches["generic"] = true
		}
		if e.tool == "mcp_dummy_lookup" {
			branches["mcp"] = true
		}
		if strings.Contains(e.tool, "_mcp-find") {
			branches["docker"] = true
		}
	}
	if len(branches) != 3 {
		t.Fatal("missing generic or MCP factory branch")
	}
}

func TestPreviewLongOutputsActuallyExpand(t *testing.T) {
	count := 0
	for _, e := range PreviewExamples() {
		if !e.expandableOutput {
			continue
		}
		count++
		t.Run(e.ID, func(t *testing.T) {
			p, _ := NewPreview()
			o := PreviewOptions{Example: e.ID, Model: "dummy-coder", Scenario: "working", Cols: 180, Rows: 75}
			if _, err := p.Render(o); err != nil {
				t.Fatal(err)
			}
			expanded := false
			for _, item := range p.exampleItems(o) {
				control, ok := item.(chat.Expandable)
				if !ok {
					continue
				}
				renders := []string{ansi.Strip(item.Render(180))}
				control.ToggleExpanded()
				renders = append(renders, ansi.Strip(item.Render(180)))
				control.ToggleExpanded()
				renders = append(renders, ansi.Strip(item.Render(180)))
				minLines, maxLines := strings.Count(renders[0], "\n"), strings.Count(renders[0], "\n")
				for _, render := range renders[1:] {
					lines := strings.Count(render, "\n")
					minLines = min(minLines, lines)
					maxLines = max(maxLines, lines)
				}
				if maxLines > minLines {
					expanded = true
				}
			}
			if !expanded {
				t.Fatal("long fixture did not expose additional lines through the real expansion path")
			}
		})
	}
	if count < 45 {
		t.Fatalf("only %d expandable examples covered", count)
	}
}

func TestPreviewOverlaysUseProductionComponents(t *testing.T) {
	for _, menu := range PreviewModals {
		if menu.ID == "none" {
			continue
		}
		t.Run("modal-"+menu.ID, func(t *testing.T) {
			p, _ := NewPreview()
			o := PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 180, Rows: 75, Modal: menu.ID}
			for _, row := range []int{0, 1} {
				o.MenuRow = row
				frame, err := p.Render(o)
				if err != nil {
					t.Fatal(err)
				}
				if menu.ID == "tasks" || menu.ID == "task-detail" {
					if p.ui.taskPanel == nil || p.ui.dialog.HasDialogs() {
						t.Fatal("tasks must use the production bottom panel, not an overlay")
					}
				} else if !p.ui.dialog.HasDialogs() {
					t.Fatal("modal not installed in real overlay")
				}
				if actual := p.ui.View().Content; frame.Content != actual {
					before, after := strings.Split(ansi.Strip(frame.Content), "\n"), strings.Split(ansi.Strip(actual), "\n")
					for line := 0; line < min(len(before), len(after)); line++ {
						if before[line] != after[line] {
							t.Fatalf("modal changed between production renders at line %d: %q -> %q", line, before[line], after[line])
						}
					}
					t.Fatal("modal bypassed production view")
				}
			}
		})
	}
	for _, menu := range PreviewPopovers {
		if menu.ID == "none" {
			continue
		}
		t.Run("popover-"+menu.ID, func(t *testing.T) {
			p, _ := NewPreview()
			frame, err := p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 180, Rows: 75, Popover: menu.ID, MenuRow: 1})
			if err != nil {
				t.Fatal(err)
			}
			if !p.ui.completionsOpen || !p.ui.completions.HasItems() {
				t.Fatal("popover missing real completion items")
			}
			if frame.Content != p.ui.View().Content {
				t.Fatal("popover bypassed production view")
			}
		})
	}
	p, _ := NewPreview()
	if _, err := p.Render(PreviewOptions{Model: "dummy-coder", Scenario: "working", Cols: 180, Rows: 75, Modal: "does-not-exist"}); err == nil {
		t.Fatal("invalid overlay silently accepted")
	}
}

func TestPreviewProviderUsesBrandedWordmark(t *testing.T) {
	p, _ := NewPreview()
	if _, err := p.Render(PreviewOptions{Model: "dummy-coder", Scenario: "working", Cols: 180, Rows: 75}); err != nil {
		t.Fatal(err)
	}
	if p.ui.brand == nil || p.ui.brand.Title != "DUMMYAI" {
		t.Fatal("dummy provider metadata not mapped through branding")
	}
	wordmark := ansi.Strip(p.ui.sidebarLogo)
	if strings.Contains(wordmark, "DUMMYAI") || !strings.ContainsAny(wordmark, "█▀▄") {
		t.Fatalf("expected provider ASCII wordmark, got %s", wordmark)
	}
}
