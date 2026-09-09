package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

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
	case "stale", "owner", "account", "operation_conflict", "check_unavailable":
		return http.StatusConflict
	case "canceled", "deadline":
		return http.StatusRequestTimeout
	case "client_runtime_managed":
		return http.StatusBadRequest
	case "mutation_failed", "check_failed":
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
