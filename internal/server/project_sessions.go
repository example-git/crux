package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/session"
)

func (c *controllerV1) handleGetProjectSessions(w http.ResponseWriter, r *http.Request) {
	if !c.server.authenticatedManagementRequest(r) {
		jsonError(w, http.StatusForbidden, "project session browsing requires authenticated TLS")
		return
	}
	path, dataDir, err := c.server.validateRemoteWorkspace(r.URL.Query().Get("path"), r.URL.Query().Get("data_dir"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			jsonError(w, http.StatusBadRequest, "invalid session offset")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	dataDir, err = config.CompletionDataDirectory(ctx, path, dataDir)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "could not resolve project data directory")
		return
	}
	dataDir, _, err = c.server.allowedWorkspacePath(dataDir, false)
	if err != nil {
		jsonError(w, http.StatusForbidden, "project data directory is outside configured workspace roots")
		return
	}
	if _, _, err := c.server.allowedWorkspacePath(filepath.Join(dataDir, "crux.db"), false); err != nil {
		jsonError(w, http.StatusForbidden, "project session database is outside configured workspace roots")
		return
	}
	result, err := session.ReadCatalog(ctx, dataDir, offset)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonEncode(w, result)
}
