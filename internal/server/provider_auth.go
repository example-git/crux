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
