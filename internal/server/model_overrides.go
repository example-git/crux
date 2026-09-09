package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
)

// handlePostWorkspaceModelOverrides changes workspace selections without persistence.
//
// @Summary Override workspace models temporarily
// @Tags config
// @Accept json
// @Produce json
// @Param id path string true "Workspace ID"
// @Param request body proto.ModelOverridesRequest true "Transient selections with exact owners"
// @Success 200 {object} config.AgentModelState
// @Failure 400 {object} proto.Error
// @Failure 403 {object} proto.Error
// @Failure 404 {object} proto.Error
// @Failure 408 {object} proto.Error
// @Failure 503 {object} proto.Error
// @Router /workspaces/{id}/config/model-overrides [post]
func (c *controllerV1) handlePostWorkspaceModelOverrides(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil || validateRuntimeJSON(body) != nil {
		jsonError(w, http.StatusBadRequest, "invalid model overrides request")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request proto.ModelOverridesRequest
	if err := decoder.Decode(&request); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid model overrides request")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF || request.State.Validate() != nil {
		jsonError(w, http.StatusBadRequest, "invalid model overrides request")
		return
	}
	state, err := c.backend.OverrideModels(r.Context(), r.PathValue("id"), request.State)
	if err != nil {
		switch {
		case errors.Is(err, backend.ErrWorkspaceNotFound), errors.Is(err, backend.ErrWorkspaceClosing):
			c.handleError(w, r, err)
		case errors.Is(err, config.ErrClientRuntimeManaged):
			jsonError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			jsonError(w, http.StatusRequestTimeout, "model overrides request canceled")
		default:
			// Configuration validation errors contain selection identity, never
			// provider responses or credentials. Preserve the explicit failure.
			jsonError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	jsonEncode(w, state)
}
