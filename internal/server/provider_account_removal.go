package server

import (
	"github.com/example-git/crux/internal/proto"
	"io"
	"net/http"
)

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
