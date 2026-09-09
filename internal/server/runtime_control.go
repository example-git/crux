package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
)

func (c *controllerV1) handleResolveWorkspaceRuntimeControl(w http.ResponseWriter, r *http.Request) {
	c.handleWorkspaceRuntimeControl(w, r, false, false)
}

func (c *controllerV1) handlePutWorkspaceRuntimeControl(w http.ResponseWriter, r *http.Request) {
	c.handleWorkspaceRuntimeControl(w, r, true, true)
}

func (c *controllerV1) handleDeleteWorkspaceRuntimeControl(w http.ResponseWriter, r *http.Request) {
	c.handleWorkspaceRuntimeControl(w, r, true, false)
}

func (c *controllerV1) handleWorkspaceRuntimeControl(w http.ResponseWriter, r *http.Request, mutate, set bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxRuntimeControlRequestBytes))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid runtime control request")
		return
	}
	request, err := proto.DecodeRuntimeControlRequest(body, mutate, set)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid runtime control request")
		return
	}
	var state config.RuntimeControlState
	switch {
	case !mutate:
		state, err = c.backend.RuntimeControlState(r.Context(), r.PathValue("id"), *request.Scope, request.Target)
	case set:
		state, err = c.backend.SetRuntimeControl(r.Context(), r.PathValue("id"), *request.Scope, request.Target, request.Value)
	default:
		state, err = c.backend.RemoveRuntimeControl(r.Context(), r.PathValue("id"), *request.Scope, request.Target)
	}
	if err != nil {
		switch {
		case errors.Is(err, backend.ErrWorkspaceNotFound), errors.Is(err, backend.ErrWorkspaceClosing):
			c.handleError(w, r, err)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			jsonError(w, http.StatusRequestTimeout, "runtime control request canceled")
		default:
			jsonError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded) > proto.MaxRuntimeControlResponseBytes || proto.ValidateRuntimeControlState(state) != nil {
		jsonError(w, http.StatusInternalServerError, "runtime control response is unavailable or exceeds its size limit")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}
