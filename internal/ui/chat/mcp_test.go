package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/styles"
)

func TestLooksLikeDiff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "simple unified diff",
			content: `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1,5 +1,6 @@
 package main
 
+import "fmt"
+
 func main() {
-    println("hello")
+    fmt.Println("hello")
 }
`,
			want: true,
		},
		{
			name:    "plain text",
			content: "This is just some plain text with no diff markers.",
			want:    false,
		},
		{
			name:    "empty string",
			content: "",
			want:    false,
		},
		{
			name: "markdown with headers",
			content: `# Title

Some content here.

## Subtitle

More content with **bold** text.
`,
			want: false,
		},
		{
			name: "diff with mixed content",
			content: `diff --git a/file.txt b/file.txt
--- a/file.txt
+++ b/file.txt
@@ -1 +1 @@
-old line
+new line
`,
			want: true,
		},
		{
			name: "only plus/minus without hunk or headers",
			content: `Hello world
---
This is not really a diff
Just some text with a few symbols
+ another line
More regular content here
And even more content
`,
			want: false,
		},
		{
			name: "GitHub PR diff format",
			content: `diff --git a/src/app.ts b/src/app.ts
index abc1234..def5678 100644
--- a/src/app.ts
+++ b/src/app.ts
@@ -10,6 +10,8 @@ function handleRequest() {
   const data = getData();
+  validate(data);
+  log(data);
   return process(data);
 }
`,
			want: true,
		},
		{
			name: "non-git unified patch with hunk and headers",
			content: `--- a/old.c
+++ b/old.c
@@ -1,3 +1,4 @@
 #include <stdio.h>
-int main() {
+int main(int argc, char **argv) {
     return 0;
 }
`,
			want: true,
		},
		{
			name: "file headers without hunk markers",
			content: `--- a/somefile.txt
+++ b/somefile.txt
Just some content here
No hunk markers at all
`,
			want: false,
		},
		{
			name: "hunk markers without file headers",
			content: `@@ -1,3 +1,4 @@
 some line
-another line
+changed line
`,
			want: false,
		},
		{
			name: "markdown list with plus signs",
			content: `- Item one
- Item two
+ Bonus item
- Item three
`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := looksLikeDiff(tt.content)
			if got != tt.want {
				t.Errorf("looksLikeDiff() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUnifiedPatchRenderingPreservesContent(t *testing.T) {
	sty := styles.CharmtonePantera()
	patches := []string{
		"--- a/main.go\n+++ b/main.go\n@@ -100 +100 @@ function\n-oldValue\n+newValue\n",
		"--- a/file.txt\n+++ b/file.txt\n@@ -100 +99,0 @@\n-movedLine\n@@ -200,0 +200 @@\n+movedLine\n",
		"diff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1 @@\n+package main\n",
		"diff --git a/old.go b/old.go\ndeleted file mode 100644\n--- a/old.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-package main\n",
		"--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n---- tricky\n++++ newer\n\\ No newline at end of file\n",
		"--- a/one.txt\n+++ b/one.txt\n@@ -1 +1 @@\n-oldOne\n+newOne\n--- a/two.txt\n+++ b/two.txt\n@@ -50 +50 @@\n-oldTwo\n+newTwo\n",
	}
	for _, patch := range patches {
		for _, width := range []int{80, 160} {
			view := ansi.Strip(toolOutputDiffContentFromUnified(&sty, patch, width, true))
			for _, line := range strings.Split(strings.TrimSuffix(patch, "\n"), "\n") {
				if len(line) > 0 && (line[0] == '+' || line[0] == '-') && !strings.HasPrefix(line, "--- ") && !strings.HasPrefix(line, "+++ ") {
					line = line[1:]
				}
				if !strings.Contains(view, line) {
					t.Fatalf("width %d omitted %q:\n%s", width, line, view)
				}
			}
		}
	}
}

func TestLooksLikeDiffVersusMarkdown(t *testing.T) {
	t.Parallel()

	// A unified diff should be detected as a diff, not markdown,
	// even though it contains "-" which could match markdown patterns.
	diffContent := strings.Join([]string{
		"diff --git a/README.md b/README.md",
		"--- a/README.md",
		"+++ b/README.md",
		"@@ -1,3 +1,3 @@",
		" # Title",
		"-Old subtitle",
		"+New subtitle",
		" Some content",
	}, "\n")

	if !looksLikeDiff(diffContent) {
		t.Error("looksLikeDiff() should detect unified diff")
	}
}
