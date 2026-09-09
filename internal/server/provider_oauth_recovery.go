package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/proto"
)

// handlePostWorkspaceOAuthLoginResults lists private owner-side result metadata.
//
// @Summary List recorded OAuth operation results
// @Description Lists pending recorded results for the exact current provider owner and target. No token, callback or account namespace is returned. Unknown exchanges remain unknown and cannot be resumed as a new exchange.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderAuthenticationTarget true "Exact current authentication target"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderOAuthLoginRecoveryListResponse
// @Failure 400 {object} proto.Error
// @Failure 403 {object} proto.Error
// @Failure 404 {object} proto.Error
// @Failure 408 {object} proto.ProviderOAuthLoginRecoveryListResponse
// @Failure 409 {object} proto.ProviderOAuthLoginRecoveryListResponse
// @Failure 422 {object} proto.ProviderOAuthLoginRecoveryListResponse
// @Failure 500 {object} proto.Error
// @Router /workspaces/{id}/auth/oauth/results [post]
func (c *controllerV1) handlePostWorkspaceOAuthLoginResults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid OAuth recovery listing request")
		return
	}
	target, err := proto.DecodeProviderOAuthLoginRecoveryListRequest(body)
	if err != nil || target.WorkspaceID != r.PathValue("id") || !target.Owner.HasOAuth {
		jsonError(w, http.StatusBadRequest, "invalid OAuth recovery listing request")
		return
	}
	response, _ := c.backend.ListProviderOAuthLoginResults(r.Context(), r.PathValue("id"), target)
	if err := response.Validate(target); err != nil {
		jsonError(w, http.StatusInternalServerError, "OAuth recovery listing is unavailable")
		return
	}
	body, err = json.Marshal(response)
	if err != nil || len(body) > proto.MaxProviderAuthResponseBytes {
		jsonError(w, http.StatusInternalServerError, "OAuth recovery listing exceeds limits")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(providerAuthResponseStatus(response.Error))
	_, _ = w.Write(body)
}

// handlePostWorkspaceOAuthLoginRecover creates a fresh authorized session from a recorded result.
//
// @Summary Recover an observed OAuth login result
// @Description Explicitly links a fresh login and operation identity to one original recorded token result. Recovery never repeats an OAuth exchange and does not claim original persistence or publication. The returned session uses the normal Complete transaction.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderOAuthLoginRecoveryRequest true "Fresh login identity and exact original recorded operation"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderOAuthLoginResponse
// @Failure 400 {object} proto.Error
// @Failure 403 {object} proto.Error
// @Failure 404 {object} proto.Error
// @Failure 408 {object} proto.ProviderOAuthLoginResponse
// @Failure 409 {object} proto.ProviderOAuthLoginResponse
// @Failure 422 {object} proto.ProviderOAuthLoginResponse
// @Failure 500 {object} proto.Error
// @Router /workspaces/{id}/auth/oauth/recover [post]
func (c *controllerV1) handlePostWorkspaceOAuthLoginRecover(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid OAuth recovery request")
		return
	}
	request, err := proto.DecodeProviderOAuthLoginRecoverRequest(body)
	if err != nil || request.Login.Target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid OAuth recovery request")
		return
	}
	response, _ := c.backend.RecoverProviderOAuthLogin(r.Context(), r.PathValue("id"), request)
	if err := response.ValidateRecover(request); err != nil {
		jsonError(w, http.StatusInternalServerError, "OAuth recovery response is unavailable")
		return
	}
	writeProviderOAuthLoginResponse(w, response)
}
