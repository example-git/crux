package server

import (
	"errors"
	"net/http"

	"github.com/example-git/crux/internal/config"

	"github.com/example-git/crux/internal/proto"
)

func (c *controllerV1) handleGetConfigReadView(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workspaceID := r.PathValue("id")
	view, err := c.backend.ReadConfigView(r.Context(), workspaceID)
	if err != nil {
		if errors.Is(err, config.ErrClientRuntimeManaged) {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, proto.ConfigReadResponse{WorkspaceID: workspaceID, View: view})
}
