package attachmenthttp

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/example-git/crux/internal/proto"
)

type Backend interface {
	GetWorkingDir(string) (string, error)
}

func Handler(backend Backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Path string `json:"path"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Path == "" {
			http.Error(w, "invalid attachment path", http.StatusBadRequest)
			return
		}
		directory, err := backend.GetWorkingDir(r.PathValue("id"))
		if err != nil {
			http.Error(w, "workspace not found", http.StatusNotFound)
			return
		}
		path := request.Path
		if filepath.IsAbs(path) {
			path, err = filepath.Rel(directory, path)
		}
		if err != nil || !filepath.IsLocal(path) {
			http.Error(w, "attachment must be inside the workspace", http.StatusBadRequest)
			return
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			http.Error(w, "could not open workspace", http.StatusInternalServerError)
			return
		}
		defer root.Close()
		info, err := root.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			http.Error(w, "attachment must be a readable regular workspace file", http.StatusBadRequest)
			return
		}
		file, err := openAttachment(root, path)
		if err != nil {
			http.Error(w, "could not open attachment", http.StatusBadRequest)
			return
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			http.Error(w, "attachment changed while opening", http.StatusConflict)
			return
		}
		content, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "could not read attachment", http.StatusInternalServerError)
			return
		}
		attachment := proto.Attachment{FilePath: filepath.Join(directory, path), FileName: filepath.Base(path), MimeType: http.DetectContentType(content), Content: content}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(attachment)
	}
}
