package server

import (
	"encoding/json"
	"net/http"

	"github.com/example-git/crux/internal/proto"
)

func (c *controllerV1) handleGetWorkspaceProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := c.backend.ListProjects(r.PathValue("id"))
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, projects)
}

func (c *controllerV1) handlePostWorkspaceProjectSelection(w http.ResponseWriter, r *http.Request) {
	var request proto.ProjectSelectionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		c.server.logError(r, "Failed to decode request", "error", err)
		jsonError(w, http.StatusBadRequest, "failed to decode request")
		return
	}
	if err := c.backend.SelectProject(r.PathValue("id"), request.Slug); err != nil {
		c.handleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
