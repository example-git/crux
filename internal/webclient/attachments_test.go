package webclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/proto"
)

type attachmentFixture struct {
	directory string
}

func (f attachmentFixture) GetWorkingDir(string) (string, error) {
	return f.directory, nil
}

func TestWorkspaceFileAttachment(t *testing.T) {
	directory := t.TempDir()
	content := "# Project notes\n\nExact attachment content.\n"
	if err := os.WriteFile(filepath.Join(directory, "notes.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ui/workspaces/{id}/files/attachment", fileAttachment(attachmentFixture{directory: directory}))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/ui/workspaces/workspace/files/attachment", strings.NewReader(`{"path":"notes.md"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", response.Code)
	}
	var attachment proto.Attachment
	if err := json.Unmarshal(response.Body.Bytes(), &attachment); err != nil {
		t.Fatal(err)
	}
	if string(attachment.Content) != content || attachment.FileName != "notes.md" || attachment.FilePath != filepath.Join(directory, "notes.md") || !strings.HasPrefix(attachment.MimeType, "text/plain") {
		t.Fatal("attachment did not retain its content and metadata")
	}
}

func TestWorkspaceFileAttachmentRejectsOutsideFiles(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ui/workspaces/{id}/files/attachment", fileAttachment(attachmentFixture{directory: directory}))
	for _, path := range []string{"../outside.txt", outside, "link", "."} {
		t.Run(path, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"path": path})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/ui/workspaces/workspace/files/attachment", strings.NewReader(string(body))))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("expected rejection, got %d", response.Code)
			}
		})
	}
}
