package webclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/example-git/crux/internal/config"
)

type commandFixture struct {
	config *config.Config
}

func (f commandFixture) GetWorkspaceConfig(id string) (*config.Config, error) {
	if id != "workspace" {
		return nil, fmt.Errorf("unknown workspace")
	}
	return f.config, nil
}

func TestWorkspaceCustomCommands(t *testing.T) {
	if os.Getenv("CRUX_WEB_COMMAND_TEST_CHILD") != "1" {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
		t.Setenv("CRUX_WEB_COMMAND_TEST_CHILD", "1")
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWorkspaceCustomCommands$", "-test.count=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated command route: %v\n%s", err, output)
		}
		return
	}

	directory := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui/workspaces/{id}/commands", customCommands(commandFixture{config: &config.Config{Options: &config.Options{DataDirectory: directory}}}))
	read := func(id string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ui/workspaces/"+id+"/commands", nil))
		return response
	}
	if response := read("missing"); response.Code != http.StatusNotFound {
		t.Fatalf("missing workspace status: %d", response.Code)
	}
	if response := read("workspace"); response.Code != http.StatusOK || response.Body.String() != "[]\n" {
		t.Fatalf("empty catalog: %d %s", response.Code, response.Body.String())
	}

	projectContent := "# Review\n\nCompare $TARGET with $BASE, then revisit $TARGET. Keep $lower literal.\n"
	files := map[string]string{
		filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "crux", "commands", "global.md"): "Global command\n",
		filepath.Join(os.Getenv("HOME"), ".crux", "commands", "home.md"):          "Home command\n",
		filepath.Join(directory, "commands", "review", "changes.md"):             projectContent,
		filepath.Join(directory, "commands", "ignored.txt"):                      "Not a command\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	response := read("workspace")
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("catalog response: %d %s", response.Code, response.Header().Get("Content-Type"))
	}
	var result []customCommand
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	want := []customCommand{
		{ID: "user:global", Name: "user:global", Content: "Global command\n", Arguments: []commandArgument{}},
		{ID: "user:home", Name: "user:home", Content: "Home command\n", Arguments: []commandArgument{}},
		{ID: "project:review:changes", Name: "project:review:changes", Content: projectContent, Arguments: []commandArgument{
			{ID: "TARGET", Title: "TARGET", Required: true},
			{ID: "BASE", Title: "BASE", Required: true},
		}},
	}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("catalog mismatch: got %#v, want %#v", result, want)
	}
}
