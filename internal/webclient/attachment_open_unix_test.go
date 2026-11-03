//go:build unix

package webclient

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWorkspaceFileAttachmentRejectsFIFO(t *testing.T) {
	directory := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(directory, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ui/workspaces/{id}/files/attachment", fileAttachment(attachmentFixture{directory: directory}))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/ui/workspaces/workspace/files/attachment", strings.NewReader(`{"path":"pipe"}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected FIFO rejection, got %d", response.Code)
	}
}

func TestAttachmentOpenFIFOWithoutWriter(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "attachment")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	type opened struct {
		file *os.File
		err  error
	}
	done := make(chan opened, 1)
	go func() {
		file, err := openAttachment(root, "attachment")
		done <- opened{file, err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		defer result.file.Close()
		info, err := result.file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().IsRegular() {
			t.Fatal("FIFO must not pass the handler's regular-file check")
		}
	case <-time.After(time.Second):
		writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			writer.Close()
		}
		select {
		case result := <-done:
			if result.file != nil {
				result.file.Close()
			}
		case <-time.After(time.Second):
		}
		t.Fatal("attachment open blocked waiting for a FIFO writer")
	}
}
