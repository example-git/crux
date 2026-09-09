package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

// handlePostWorkspaceProviderSwitch documents the workspace authority contract.
//
// @Summary Switch the selected provider account
// @Description Server-owned transaction endpoint. Client-owned workspaces use the owning client transaction and publish an explicit runtime replacement. Request-bound outcomes distinguish account/configuration saves from runtime publication.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderAccountSwitchRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderAuthenticationMutationResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderAuthenticationMutationResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderAuthenticationMutationResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderAuthenticationMutationResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/switch [post]
func (c *controllerV1) handlePostWorkspaceProviderSwitch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid provider authentication switch request")
		return
	}
	request, err := proto.DecodeProviderAuthSwitchRequest(body)
	if err != nil || request.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid provider authentication switch request")
		return
	}
	response, _ := c.backend.SwitchProviderAccount(r.Context(), r.PathValue("id"), request)
	if err := response.ValidateSwitch(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "provider authentication response is unavailable")
		return
	}
	writeProviderAuthMutationResponse(w, response)
}

// handlePostWorkspaceProviderLogout documents the workspace authority contract.
//
// @Summary Log out the exact provider selection
// @Description Server-owned transaction endpoint. An operation retry retains the original target and operation identity; it cannot log out a replacement selection.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderLogoutRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderAuthenticationMutationResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderAuthenticationMutationResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderAuthenticationMutationResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderAuthenticationMutationResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/logout [post]
func (c *controllerV1) handlePostWorkspaceProviderLogout(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid provider authentication logout request")
		return
	}
	request, err := proto.DecodeProviderAuthLogoutRequest(body)
	if err != nil || request.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid provider authentication logout request")
		return
	}
	response, _ := c.backend.LogoutProvider(r.Context(), r.PathValue("id"), request)
	if err := response.ValidateLogout(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "provider authentication response is unavailable")
		return
	}
	writeProviderAuthMutationResponse(w, response)
}

func writeProviderAuthMutationResponse(w http.ResponseWriter, response proto.ProviderAuthenticationMutationResponse) {
	body, err := json.Marshal(response)
	if err != nil || len(body) > proto.MaxProviderAuthResponseBytes {
		// The complete receipt/view may exceed the wire budget. Keep truthful
		// request-bound progress; never reduce a known publication to a bare error.
		response.Workspace = nil
		response.Outcome.Change = nil
		response.Outcome.Superseded = false
		response.Error = proto.NewProviderAuthenticationError(providerauth.ErrReceiptUnverified)
		body, err = json.Marshal(response)
		if err != nil || len(body) > proto.MaxProviderAuthResponseBytes {
			jsonError(w, http.StatusInternalServerError, "provider authentication response exceeds limits")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(providerAuthResponseStatus(response.Error))
	_, _ = w.Write(body)
}

func providerAuthResponseStatus(failure *proto.ProviderAuthenticationError) int {
	if failure == nil {
		return http.StatusOK
	}
	switch failure.Code {
	case "stale", "owner", "account", "operation_conflict", "check_unavailable", "oauth_login_unavailable":
		return http.StatusConflict
	case "canceled", "deadline":
		return http.StatusRequestTimeout
	case "client_runtime_managed":
		return http.StatusBadRequest
	case "mutation_failed", "check_failed", "oauth_login_failed":
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
