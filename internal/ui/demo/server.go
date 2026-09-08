// Package demo serves the real TUI fixtures and embedded xterm client.
package demo

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/model"
	"path/filepath"
)

//go:generate npm --prefix ../../../tools/tui-mock run build:web
//go:embed web
var assets embed.FS

func NewHandler() (http.Handler, error) { return NewHandlerWithProject("", "") }
func NewHandlerWithProject(project, initialSession string) (http.Handler, error) {
	return newHandler(project, initialSession, true)
}
func newHandler(project, initialSession string, loadRuntime bool) (http.Handler, error) {
	source, err := newSessionSource(project, initialSession)
	if err != nil {
		return nil, err
	}
	if source.project != "" && loadRuntime {
		store, err := config.LoadPreview(source.project, filepath.Join(source.project, ".crux"))
		if err != nil {
			return nil, fmt.Errorf("load normal demo providers: %w", err)
		}
		source.runtimeConfig = store.Config()
	}
	preview, err := model.NewPreview()
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	mux := http.NewServeMux()
	registerSchemaHelp(mux)
	registerControl(mux, func(o model.PreviewOptions) (model.PreviewFrame, error) {
		mu.Lock()
		defer mu.Unlock()
		p := preview
		if o.ImportID != "" {
			var err error
			p, err = source.preview(o.ImportID)
			if err != nil {
				return model.PreviewFrame{}, err
			}
		}
		return p.Render(o)
	})
	source.register(mux)
	mux.HandleFunc("GET /api/fixture", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := preview
		if id := r.URL.Query().Get("importId"); id != "" {
			var err error
			p, err = source.preview(id)
			if err != nil {
				http.Error(w, `{"error":"unknown snapshot"}`, 400)
				return
			}
		}
		json.NewEncoder(w).Encode(p.FixtureData())
	})
	mux.HandleFunc("GET /api/catalog", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if id := r.URL.Query().Get("importId"); id != "" {
			p, err := source.preview(id)
			if err != nil {
				http.Error(w, `{"error":"unknown snapshot"}`, 400)
				return
			}
			json.NewEncoder(w).Encode(p.Catalog())
			return
		}
		json.NewEncoder(w).Encode(model.PreviewRegistry())
	})
	mux.HandleFunc("POST /api/preview", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		var o model.PreviewOptions
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&o); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		mu.Lock()
		defer mu.Unlock()
		defer func() {
			if failure := recover(); failure != nil {
				log.Printf("preview render panic: %v", failure)
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"error": "Go renderer failed; check the preview server log"})
			}
		}()
		p := preview
		if o.ImportID != "" {
			var err error
			p, err = source.preview(o.ImportID)
			if err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
		}
		frame, err := p.Render(o)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(frame)
	})
	web, err := fs.Sub(assets, "web")
	if err != nil {
		return nil, fmt.Errorf("demo assets: %w", err)
	}
	files := http.FileServer(http.FS(web))
	help := func(w http.ResponseWriter, r *http.Request) {
		name := "index.md"
		if strings.HasPrefix(r.URL.Path, "/help/") && r.URL.Path != "/help/" {
			name = strings.TrimPrefix(r.URL.Path, "/help/")
		}
		if strings.Contains(name, "/") || !strings.HasSuffix(name, ".md") {
			http.NotFound(w, r)
			return
		}
		data, err := assets.ReadFile("web/help/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(data)
	}
	mux.HandleFunc("GET /help", help)
	mux.HandleFunc("GET /help/", help)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Vary", "Accept")
			accept := r.Header.Get("Accept")
			if !strings.Contains(accept, "text/html") || strings.Contains(accept, "text/markdown") {
				if r.Method != http.MethodGet && r.Method != http.MethodHead {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				help(w, r)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
	return mux, nil
}
