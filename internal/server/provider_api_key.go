package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/proto"
)

// handlePostWorkspaceAPIKeyCheck documents the workspace authority contract.
//
// @Summary Check a selected credential field
// @Description Retains the exact owner, credential slot and submitted value under a check identity. Checking does not save a credential or change selected models. The response reports the check evidence.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderAPIKeyCheckRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderAPIKeyCheckResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderAPIKeyCheckResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderAPIKeyCheckResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderAPIKeyCheckResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/api-key/check [post]
func (c *controllerV1) handlePostWorkspaceAPIKeyCheck(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAPIKeyCheckRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid API key check request")
		return
	}
	request, err := proto.DecodeProviderAPIKeyCheckRequest(body)
	if err != nil || request.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid API key check request")
		return
	}
	response, _ := c.backend.CheckProviderAPIKey(r.Context(), r.PathValue("id"), request)
	if err := response.Validate(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "API key check response is unavailable")
		return
	}
	body, err = json.Marshal(response)
	if err != nil || len(body) > proto.MaxProviderAuthResponseBytes {
		jsonError(w, http.StatusInternalServerError, "API key check response exceeds limits")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(providerAuthResponseStatus(response.Error))
	_, _ = w.Write(body)
}

// handlePostWorkspaceAPIKeySave documents the workspace authority contract.
//
// @Summary Save the exact checked credential
// @Description Consumes the retained check for the requested target and operation. Save does not repeat the check probe. Client-owned persistence and runtime replacement occur on the owning client.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderAPIKeySaveRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderAuthenticationMutationResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderAuthenticationMutationResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderAuthenticationMutationResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderAuthenticationMutationResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/api-key/save [post]
func (c *controllerV1) handlePostWorkspaceAPIKeySave(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid checked API key save request")
		return
	}
	request, err := proto.DecodeProviderAPIKeySaveRequest(body)
	if err != nil || request.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid checked API key save request")
		return
	}
	response, _ := c.backend.SaveCheckedProviderAPIKey(r.Context(), r.PathValue("id"), request)
	if err := response.ValidateAPIKeySave(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "checked API key response is unavailable")
		return
	}
	writeProviderAuthMutationResponse(w, response)
}
