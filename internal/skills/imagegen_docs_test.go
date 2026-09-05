package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImagegenChromaHelperDocumentedInvocationMatchesHelp(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}

	script := filepath.Join("builtin", "imagegen", "scripts", "remove_chroma_key.py")
	output, err := exec.CommandContext(t.Context(), python, script, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("chroma helper --help: %v\n%s", err, output)
	}
	help := string(output)
	for _, flag := range []string{"--input", "--out"} {
		if !strings.Contains(help, flag) {
			t.Errorf("helper help does not contain %s", flag)
		}
	}

	docsPath := filepath.Join("builtin", "imagegen", "references", "image-api.md")
	docs, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("read image API docs: %v", err)
	}
	want := "remove_chroma_key.py --input <input> --out <output>"
	if !strings.Contains(string(docs), want) {
		t.Fatalf("documentation does not contain supported invocation %q", want)
	}
}
