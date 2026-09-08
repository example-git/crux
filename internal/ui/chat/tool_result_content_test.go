package chat

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/ui/styles"
)

func TestHumanizedToolName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "snake case", input: "mcp_github_get", want: "Mcp Github Get"},
		{name: "kebab case", input: "web-fetch", want: "Web Fetch"},
		{name: "mixed", input: "job_output-tool", want: "Job Output Tool"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := humanizedToolName(tt.input); got != tt.want {
				t.Fatalf("humanizedToolName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLooksLikeMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{name: "header", content: "# Title", want: true},
		{name: "code fence", content: "```go\nfmt.Println(\"x\")\n```", want: true},
		{name: "plain", content: "hello world", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := looksLikeMarkdown(tt.content); got != tt.want {
				t.Fatalf("looksLikeMarkdown() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRenderToolResultTextContent(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	styPtr := &sty
	width := 82
	bodyWidth := toolBodyWidth(styPtr, width)

	t.Run("json branch", func(t *testing.T) {
		t.Parallel()
		content := `{"a":1}`
		var result json.RawMessage
		if err := json.Unmarshal([]byte(content), &result); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		prettyResult, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			t.Fatalf("json.MarshalIndent() error = %v", err)
		}
		expected := toolOutputCodeContent(styPtr, "result.json", string(prettyResult), 0, width, false)
		got := renderToolResultTextContent(styPtr, content, width, false)
		if got != expected {
			t.Fatal("renderToolResultTextContent() did not choose JSON rendering")
		}
	})

	t.Run("diff branch before markdown", func(t *testing.T) {
		t.Parallel()
		content := `diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-# Old
+# New
`
		expected := toolOutputDiffContentFromUnified(styPtr, content, width, false)
		got := renderToolResultTextContent(styPtr, content, width, false)
		if got != expected {
			t.Fatal("renderToolResultTextContent() did not choose diff rendering")
		}
	})

	t.Run("markdown branch", func(t *testing.T) {
		t.Parallel()
		content := "# Title\n\nBody"
		expected := styPtr.Tool.Body.Render(toolOutputMarkdownPanel(styPtr, content, bodyWidth, false))
		got := renderToolResultTextContent(styPtr, content, width, false)
		if got != expected {
			t.Fatal("renderToolResultTextContent() did not choose markdown rendering")
		}
	})

	t.Run("plain branch", func(t *testing.T) {
		t.Parallel()
		content := "plain text"
		expected := styPtr.Tool.Body.Render(toolOutputPlainContent(styPtr, content, bodyWidth, false))
		got := renderToolResultTextContent(styPtr, content, width, false)
		if got != expected {
			t.Fatal("renderToolResultTextContent() did not choose plain rendering")
		}
	})
}
