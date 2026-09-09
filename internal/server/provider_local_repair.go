package server

import (
	"encoding/json"
	"github.com/example-git/crux/internal/proto"
	"io"
	"net/http"
)

// handlePostWorkspaceLocalRepair reviews or applies one exact historical local
// authentication disk operation. It never publishes a runtime or returns secrets.
// @Summary Review or repair an original authentication disk operation
// @Tags providers
// @Accept json
// @Produce json
// @Param id path string true "Current workspace authorized for this principal"
// @Param request body providerauth.LocalRepairRequest true "Historical operation and reviewed revision"
// @Success 200 {object} proto.ProviderLocalRepairResponse
// @Failure 422 {object} proto.ProviderLocalRepairResponse
// @Router /workspaces/{id}/auth/local-repair [post]
func (c *controllerV1) handlePostWorkspaceLocalRepair(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid local authentication repair request")
		return
	}
	request, err := proto.DecodeProviderLocalRepairRequest(raw)
	if err != nil || request.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid local authentication repair request")
		return
	}
	response, _ := c.backend.RepairLocalAuthentication(r.Context(), r.PathValue("id"), request)
	if err := response.Validate(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "local authentication repair response is unavailable")
		return
	}
	body, err := json.Marshal(response)
	if err != nil || len(body) > proto.MaxProviderAuthResponseBytes {
		jsonError(w, http.StatusInternalServerError, "local authentication repair response exceeds limits")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	if response.Error != nil {
		status = http.StatusUnprocessableEntity
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
