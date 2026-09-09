package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/proto"
)

// handleGetWorkspaceProviderAuthentication documents the workspace authority contract.
//
// @Summary Get workspace authentication status
// @Description Returns redacted provider owners, credential choices and authority generation. Status is not proof of a completed login or publication. No request body is accepted.
// @Tags providers
// @Produce json
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderAuthenticationSnapshot
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.Error "Request canceled"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth [get]
func (c *controllerV1) handleGetWorkspaceProviderAuthentication(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
		if err != nil || len(body) != 0 {
			jsonError(w, http.StatusBadRequest, "provider authentication status does not accept a body")
			return
		}
	}
	state, err := c.backend.ProviderAuthentication(r.Context(), r.PathValue("id"))
	if err != nil {
		c.providerAuthError(w, r, err)
		return
	}
	writeProviderAuthResponse(w, state)
}

// handlePostWorkspaceProviderAccounts documents the workspace authority contract.
//
// @Summary List accounts for an exact authentication target
// @Description The target includes workspace, accepted authority and provider owner. Client-owned account enumeration is handled on the owning client; this remote service does not substitute server accounts.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.ProviderAuthenticationTarget true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} proto.ProviderAccountsState
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.Error "Request canceled"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/auth/accounts [post]
func (c *controllerV1) handlePostWorkspaceProviderAccounts(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid provider account request")
		return
	}
	target, err := proto.DecodeProviderAuthTarget(body)
	if err != nil || target.WorkspaceID != r.PathValue("id") {
		jsonError(w, http.StatusBadRequest, "invalid provider account request")
		return
	}
	state, err := c.backend.ProviderAccounts(r.Context(), r.PathValue("id"), target)
	if err != nil {
		c.providerAuthError(w, r, err)
		return
	}
	writeProviderAuthResponse(w, state)
}

func (c *controllerV1) providerAuthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, backend.ErrWorkspaceNotFound), errors.Is(err, backend.ErrWorkspaceClosing):
		c.handleError(w, r, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		jsonError(w, http.StatusRequestTimeout, "provider authentication request canceled")
	default:
		jsonError(w, http.StatusBadRequest, err.Error())
	}
}

func writeProviderAuthResponse(w http.ResponseWriter, state any) {
	body, err := json.Marshal(state)
	if err != nil || len(body) > proto.MaxProviderAuthResponseBytes {
		jsonError(w, http.StatusInternalServerError, "provider authentication response exceeds limits")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
