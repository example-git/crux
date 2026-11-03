package webclient

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/example-git/crux/internal/config"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/redact"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/version"
)

//go:embed dist
var assets embed.FS

type Startup struct {
	Path      string   `json:"path"`
	DataDir   string   `json:"data_dir,omitempty"`
	Debug     bool     `json:"debug"`
	YOLO      bool     `json:"yolo"`
	Channels  []string `json:"channels,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Continue  bool     `json:"continue"`
}

func Listen(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid web listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("web listen address must use a loopback IP, got %q", host)
	}
	return net.Listen("tcp", address)
}

type Host struct {
	http.Handler
	remote *remoteWorkspaces
}

func (h *Host) Close() error {
	return h.remote.Close()
}

func Handler(native *server.Server, address string, startup Startup) *Host {
	nativeHandler := native.Handler()
	remote := &remoteWorkspaces{native: native}
	files, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	static := http.FileServerFS(files)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui/project-sessions", projectSessions)
	mux.HandleFunc("GET /ui/connections", savedConnections)
	mux.Handle("/ui/connections/{name}/v1/", remote)
	mux.HandleFunc("GET /ui/connections/{name}/browser", connectionRead(false))
	mux.HandleFunc("GET /ui/connections/{name}/project-sessions", connectionRead(true))
	mux.HandleFunc("POST /ui/workspaces/{id}/oauth/loopback", oauthLoopback(native.Backend()))
	mux.HandleFunc("POST /ui/workspaces/{id}/files/attachment", fileAttachment(native.Backend()))
	mux.HandleFunc("GET /ui/workspaces/{id}/commands", customCommands(native.Backend()))
	mux.HandleFunc("GET /ui/workspaces/{id}/terminal", terminal(native.Backend()))
	mux.HandleFunc("GET /ui/tmux-sessions", terminalSessions())
	instructions := instructionFiles(native.Backend())
	mux.HandleFunc("GET /ui/workspaces/{id}/instructions", instructions)
	mux.HandleFunc("PUT /ui/workspaces/{id}/instructions", instructions)
	mux.HandleFunc("GET /ui/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, struct {
			Startup
			Version      string `json:"version"`
			SettingsPath string `json:"settings_path"`
		}{startup, version.Version, config.GlobalConfigData()})
	})
	mux.HandleFunc("GET /ui/workspaces/{id}/pending", func(w http.ResponseWriter, r *http.Request) {
		ws, err := native.Backend().GetWorkspace(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"permission": ws.Permissions.Pending(), "question": ws.Questions.Pending()})
	})
	mux.Handle("/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/channel") && r.Header.Get("Upgrade") != "" {
			r.Header.Set("Crux-Runtime-Protocol", proto.RemoteRuntimeProtocol)
			r.Header.Set(cruxlog.EphemeralStateHeader, "web-workspace-channel")
		}
		nativeHandler.ServeHTTP(w, r)
	}))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		static.ServeHTTP(w, r)
	}))
	return &Host{remote: remote, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self' ws://"+address+"; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if r.Host != address || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "web client requires its local origin", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != "http" || u.Host != address || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
				http.Error(w, "web client requires its local origin", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})}
}

func writeJSON(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err == nil {
		data, err = redact.JSON(data)
	}
	if err != nil {
		http.Error(w, "could not encode web client state", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
