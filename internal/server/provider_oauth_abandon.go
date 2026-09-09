package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/proto"
)

// handlePostWorkspaceOAuthLoginAbandon retires a tokenless preparation or unknown exchange.
//
// @Summary Abandon a tokenless OAuth operation
// @Description Explicitly abandons the exact original workspace and operation within the current owner and captured scope. An operation lease prevents racing an active exchange or result write. Recorded tokens cannot be abandoned here. The original outcome remains not-started or unknown; no login success, local credential save or runtime acknowledgement is implied. Retained evidence becomes eligible for bounded history pruning.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderOAuthLoginAbandonRequest true "Current target and exact original tokenless operation"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderOAuthLoginAbandonResponse
// @Failure 400 {object} proto.Error
// @Failure 403 {object} proto.Error
// @Failure 404 {object} proto.Error
// @Failure 408 {object} proto.ProviderOAuthLoginAbandonResponse
// @Failure 409 {object} proto.ProviderOAuthLoginAbandonResponse
// @Failure 422 {object} proto.ProviderOAuthLoginAbandonResponse
// @Failure 500 {object} proto.Error
// @Router /workspaces/{id}/auth/oauth/abandon [post]
func (c *controllerV1) handlePostWorkspaceOAuthLoginAbandon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid OAuth abandonment request")
		return
	}
	request, err := proto.DecodeProviderOAuthLoginAbandonRequest(body)
	if err != nil || request.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid OAuth abandonment request")
		return
	}
	response, _ := c.backend.AbandonProviderOAuthLoginResult(r.Context(), r.PathValue("id"), request)
	if err := response.Validate(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "OAuth abandonment response is unavailable")
		return
	}
	body, err = json.Marshal(response)
	if err != nil || len(body) > proto.MaxProviderAuthResponseBytes {
		jsonError(w, http.StatusInternalServerError, "OAuth abandonment response exceeds limits")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(providerAuthResponseStatus(response.Error))
	_, _ = w.Write(body)
}
