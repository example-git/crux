package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
)

// handlePostWorkspaceProviderUsage fetches normalized usage using workspace authority.
//
// @Summary Fetch provider quota usage
// @Tags providers
// @Accept json
// @Produce json
// @Param id path string true "Workspace ID"
// @Param request body config.ProviderUsageRequest true "Selected owner and accepted runtime"
// @Success 200 {object} config.ProviderUsageResult
// @Failure 400 {object} proto.Error
// @Failure 403 {object} proto.Error
// @Failure 409 {object} proto.Error
// @Failure 502 {object} proto.Error
// @Router /workspaces/{id}/providers/usage [post]
func (c *controllerV1) handlePostWorkspaceProviderUsage(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil || validateRuntimeJSON(body) != nil {
		jsonError(w, http.StatusBadRequest, "invalid provider usage request")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request config.ProviderUsageRequest
	if err := decoder.Decode(&request); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid provider usage request")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF || request.Owner.ProviderID == "" {
		jsonError(w, http.StatusBadRequest, "invalid provider usage request")
		return
	}
	result, err := c.backend.ProviderUsage(r.Context(), r.PathValue("id"), request)
	if err != nil {
		switch {
		case errors.Is(err, config.ErrProviderUsageAuthority), errors.Is(err, config.ErrProviderUsageOwner), errors.Is(err, config.ErrProviderUsageCredential):
			jsonError(w, http.StatusConflict, "provider usage selection changed")
		case errors.Is(err, backend.ErrWorkspaceNotFound), errors.Is(err, backend.ErrWorkspaceClosing):
			c.handleError(w, r, err)
		default:
			// Provider errors may contain response bodies or endpoint secrets.
			jsonError(w, http.StatusBadGateway, "provider usage request failed")
		}
		return
	}
	jsonEncode(w, result)
}
