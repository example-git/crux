package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/proto"
)

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
