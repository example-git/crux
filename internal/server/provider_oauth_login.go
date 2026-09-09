package server

import (
	"encoding/json"
	"github.com/example-git/crux/internal/proto"
	"io"
	"net/http"
)

func (c *controllerV1) handlePostWorkspaceOAuthLoginBegin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	request, err := proto.DecodeProviderOAuthLoginBeginRequest(body)
	if err != nil || request.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	response, _ := c.backend.BeginProviderOAuthLogin(r.Context(), r.PathValue("id"), request)
	if err := response.ValidateBegin(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "OAuth login response is unavailable")
		return
	}
	writeProviderOAuthLoginResponse(w, response)
}

func (c *controllerV1) handlePostWorkspaceOAuthLoginBind(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	request, err := proto.DecodeProviderOAuthLoginBindRequest(body)
	if err != nil || request.Login.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	response, _ := c.backend.BindProviderOAuthLogin(r.Context(), r.PathValue("id"), request)
	if err := response.ValidateBind(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "OAuth login response is unavailable")
		return
	}
	writeProviderOAuthLoginResponse(w, response)
}

func (c *controllerV1) handlePostWorkspaceOAuthLoginCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderOAuthCodeRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	request, err := proto.DecodeProviderOAuthLoginCodeRequest(body)
	if err != nil || request.Login.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	response, _ := c.backend.SubmitProviderOAuthLoginCode(r.Context(), r.PathValue("id"), request)
	if err := response.ValidateCode(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "OAuth login response is unavailable")
		return
	}
	writeProviderOAuthLoginResponse(w, response)
}

func (c *controllerV1) handlePostWorkspaceOAuthLoginWait(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	request, err := proto.DecodeProviderOAuthLoginWaitRequest(body)
	if err != nil || request.Login.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	response, _ := c.backend.WaitProviderOAuthLogin(r.Context(), r.PathValue("id"), request.Login, request.After)
	if err := response.ValidateWait(request.Login, request.After); err != nil {
		jsonError(w, http.StatusInternalServerError, "OAuth login response is unavailable")
		return
	}
	writeProviderOAuthLoginResponse(w, response)
}

func (c *controllerV1) handlePostWorkspaceOAuthLoginCancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	request, err := proto.DecodeProviderOAuthLoginCancelRequest(body)
	if err != nil || request.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	response, _ := c.backend.CancelProviderOAuthLogin(r.Context(), r.PathValue("id"), request)
	if err := response.ValidateCancel(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "OAuth login response is unavailable")
		return
	}
	writeProviderOAuthLoginResponse(w, response)
}

func (c *controllerV1) handlePostWorkspaceOAuthLoginComplete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	request, err := proto.DecodeProviderOAuthLoginCompleteRequest(body)
	if err != nil || request.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid OAuth login request")
		return
	}
	response, _ := c.backend.CompleteProviderOAuthLogin(r.Context(), r.PathValue("id"), request)
	if err := response.ValidateOAuthLogin(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "OAuth login response is unavailable")
		return
	}
	writeProviderAuthMutationResponse(w, response)
}

func writeProviderOAuthLoginResponse(w http.ResponseWriter, response proto.ProviderOAuthLoginResponse) {
	body, err := json.Marshal(response)
	if err != nil || len(body) > proto.MaxProviderAuthResponseBytes {
		jsonError(w, http.StatusInternalServerError, "OAuth login response exceeds limits")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(providerAuthResponseStatus(response.Error))
	_, _ = w.Write(body)
}
