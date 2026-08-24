package server

import (
	"encoding/json"
	"net/http"

	"github.com/example-git/crux/internal/proto"
)

func (c *controllerV1) handleGetWorkspaceCodebaseIndex(w http.ResponseWriter, r *http.Request) {
	status, err := c.backend.CodebaseIndexStatus(r.PathValue("id"))
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, status)
}

func (c *controllerV1) handlePostWorkspaceCodebaseIndex(w http.ResponseWriter, r *http.Request) {
	var update proto.CodebaseIndexUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		c.server.logError(r, "Failed to decode request", "error", err)
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	status, err := c.backend.UpdateCodebaseIndex(r.PathValue("id"), update)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, status)
}
