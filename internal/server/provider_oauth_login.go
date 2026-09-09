package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/proto"
)

// handlePostWorkspaceOAuthLoginBegin documents the workspace authority contract.
//
// @Summary Prepare an OAuth login
// @Description Prepares one exact provider-owned interaction. Beginning an interaction does not prove authorization, local persistence or runtime publication.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderOAuthLoginRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderOAuthLoginResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderOAuthLoginResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderOAuthLoginResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderOAuthLoginResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/oauth/begin [post]
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

// handlePostWorkspaceOAuthLoginBind documents the workspace authority contract.
//
// @Summary Bind an OAuth login callback
// @Description Binds the retained login to the exact callback binding and port before authorization starts.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderOAuthLoginBindRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderOAuthLoginResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderOAuthLoginResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderOAuthLoginResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderOAuthLoginResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/oauth/bind [post]
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

// handlePostWorkspaceOAuthLoginCode documents the workspace authority contract.
//
// @Summary Submit input to the retained OAuth login
// @Description Input is private and is never echoed in the response. The submission identity binds duplicate delivery to the same interaction.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderOAuthLoginCodeRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderOAuthLoginResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderOAuthLoginResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderOAuthLoginResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderOAuthLoginResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/oauth/code [post]
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

// handlePostWorkspaceOAuthLoginWait documents the workspace authority contract.
//
// @Summary Wait for OAuth interaction progress
// @Description Returns progress after the requested sequence for the exact retained login. An authorized interaction still requires the completion transaction.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderOAuthLoginWaitRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderOAuthLoginResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderOAuthLoginResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderOAuthLoginResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderOAuthLoginResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/oauth/wait [post]
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

// handlePostWorkspaceOAuthLoginCancel documents the workspace authority contract.
//
// @Summary Cancel the retained OAuth interaction
// @Description Cancellation applies to the retained login and preserves any known outcome; it does not silently replace the selected account.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderOAuthLoginRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderOAuthLoginResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderOAuthLoginResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderOAuthLoginResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderOAuthLoginResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/oauth/cancel [post]
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

// handlePostWorkspaceOAuthLoginComplete documents the workspace authority contract.
//
// @Summary Complete the OAuth persistence transaction
// @Description Persists the observed login result and returns exact account/configuration/publication progress. Retry the original operation or explicitly review saved state; do not infer success from interaction completion alone.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderOAuthLoginRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderAuthenticationMutationResponse
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.ProviderAuthenticationMutationResponse "Canceled or deadline exceeded; inspect retained progress"
// @Failure 409 {object} proto.ProviderAuthenticationMutationResponse "Target changed or retained operation is unavailable"
// @Failure 422 {object} proto.ProviderAuthenticationMutationResponse "Operation failed; inspect exact saved/publication progress"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/oauth/complete [post]
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
