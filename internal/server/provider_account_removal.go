package server

import (
	"github.com/example-git/crux/internal/proto"
	"io"
	"net/http"
)

// handlePostWorkspaceProviderRemove documents the workspace authority contract.
//
// @Summary Remove an exact saved provider account
// @Description Removing an inactive account requires account persistence only. Removing the active account also requires configuration persistence and runtime publication. Client-owned changes execute on the owning client.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderAccountRemoveRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderAuthenticationMutationResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderAuthenticationMutationResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderAuthenticationMutationResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderAuthenticationMutationResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/remove [post]
func (c *controllerV1) handlePostWorkspaceProviderRemove(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid provider authentication remove request")
		return
	}
	request, err := proto.DecodeProviderAuthRemoveRequest(body)
	if err != nil || request.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid provider authentication remove request")
		return
	}
	response, _ := c.backend.RemoveProviderAccount(r.Context(), r.PathValue("id"), request)
	if err := response.ValidateRemove(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "provider authentication response is unavailable")
		return
	}
	writeProviderAuthMutationResponse(w, response)
}
