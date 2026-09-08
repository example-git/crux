package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestPreviewCompactFooterPrioritizesDiscoverability(t *testing.T) {
	p, err := NewPreview()
	if err != nil {
		t.Fatal(err)
	}
	frame, err := p.Render(PreviewOptions{Cols: 65, Rows: 25, Model: "dummy-coder", Scenario: "working", Example: "status-error"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(ansi.Strip(frame.Content), "\n") {
		if strings.Contains(line, "esc cancel") {
			found = true
			if !strings.Contains(line, "commands") || !strings.Contains(line, "ctrl+g more") || !strings.Contains(line, "…") {
				t.Fatalf("incomplete compact footer: %s", line)
			}
		}
	}
	if !found {
		t.Fatal("missing footer")
	}
}

func TestPreviewEmptyListsAndPermissionCommand(t *testing.T) {
	for _, test := range []struct{ modal, data, want string }{
		{"sessions", `{"sessions":[]}`, "No saved sessions."},
		{"tasks", `{"tasks":[]}`, "No background tasks"},
		{"permissions", `{}`, "go test ./fixture -v"},
	} {
		t.Run(test.modal, func(t *testing.T) {
			p, err := NewPreview()
			if err != nil {
				t.Fatal(err)
			}
			frame, err := p.Render(PreviewOptions{Cols: 100, Rows: 40, Model: "dummy-coder", Scenario: "working", Modal: test.modal, Data: json.RawMessage(test.data)})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(ansi.Strip(frame.Content), test.want) {
				t.Fatalf("missing %q", test.want)
			}
		})
	}
}

func TestPreviewDataEditsAndReset(t *testing.T) {
	p, err := NewPreview()
	if err != nil {
		t.Fatal(err)
	}
	o := PreviewOptions{Cols: 180, Rows: 60, Model: "dummy-coder", Scenario: "working", Example: "tool-bash", Modal: "none", Popover: "none"}
	original, err := p.Render(o)
	if err != nil {
		t.Fatal(err)
	}
	o.Data = json.RawMessage(`{"session":{"Title":"DATA EDIT SENTINEL"},"examples":{"tool-bash":{"result":{"content":"CUSTOM TOOL OUTPUT"}}}}`)
	// Native session and tool structs have explicit JSON field names.
	frame, err := p.Render(o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frame.Content, "DATA EDIT SENTINEL") || !strings.Contains(frame.Content, "CUSTOM TOOL OUTPUT") {
		t.Fatalf("edited data absent from native output")
	}
	o.Data = json.RawMessage(`{"session":{"not_a_field":1}}`)
	if _, err := p.Render(o); err == nil {
		t.Fatal("unknown data field accepted")
	}
	o.Data = nil
	reset, err := p.Render(o)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Content != original.Content {
		t.Fatal("reset did not restore original dummy frame")
	}
	o.Example = "all"
	all, err := p.Render(o)
	if err != nil {
		t.Fatal(err)
	}
	o.FocusItem = all.Items[len(all.Items)-1].ID
	if _, err := p.Render(o); err != nil {
		t.Fatal(err)
	}
	o.FocusItem = "missing-item"
	if _, err := p.Render(o); err == nil {
		t.Fatal("unknown item accepted")
	}
}
