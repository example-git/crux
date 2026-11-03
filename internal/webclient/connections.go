package webclient

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
)

func savedConnections(w http.ResponseWriter, r *http.Request) {
	connections, err := connection.List(r.Context())
	if err != nil {
		http.Error(w, "could not read saved connections", http.StatusInternalServerError)
		return
	}
	type summary struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	}
	result := make([]summary, 0, len(connections))
	for _, value := range connections {
		result = append(result, summary{Name: value.Name, Address: value.Address})
	}
	writeJSON(w, result)
}

func connectionRead(catalog bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		name := r.PathValue("name")
		saved, exists, err := connection.Get(ctx, name)
		if err != nil {
			http.Error(w, "could not read selected connection", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "saved connection not found", http.StatusNotFound)
			return
		}
		remote, err := client.NewAuthenticatedClient(config.GlobalWorkspaceDir(), saved)
		if err != nil {
			http.Error(w, "could not initialize selected connection", http.StatusBadGateway)
			return
		}
		defer remote.CloseIdleConnections()
		if !catalog {
			result, err := remote.Browse(ctx, r.URL.Query().Get("path"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			writeJSON(w, result)
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
		result, err := remote.ProjectSessions(ctx, r.URL.Query().Get("path"), r.URL.Query().Get("data_dir"), offset)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, result)
	}
}
