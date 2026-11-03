package webclient

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/session"
)

type projectSession = session.CatalogEntry

func projectSessions(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !filepath.IsAbs(path) {
		http.Error(w, "project directory must be absolute", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		http.Error(w, "project directory is unavailable", http.StatusNotFound)
		return
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			http.Error(w, "invalid session offset", http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	dataDir, err := config.CompletionDataDirectory(ctx, path, r.URL.Query().Get("data_dir"))
	if err != nil {
		http.Error(w, "could not resolve project data directory", http.StatusInternalServerError)
		return
	}
	result, err := session.ReadCatalog(ctx, dataDir, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}
