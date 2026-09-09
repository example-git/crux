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

// handleResolveWorkspaceRuntimeControl documents the workspace authority contract.
//
// @Summary Resolve a declared runtime control
// @Description Resolves the exact owner/control and scope, without accepting an arbitrary configuration path.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.RuntimeControlRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} config.RuntimeControlState
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.Error "Request canceled"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/config/runtime-control/resolve [post]
func (c *controllerV1) handleResolveWorkspaceRuntimeControl(w http.ResponseWriter, r *http.Request) {
	c.handleWorkspaceRuntimeControl(w, r, false, false)
}

// handlePutWorkspaceRuntimeControl documents the workspace authority contract.
//
// @Summary Set a declared runtime control
// @Description Requires an explicit primitive value and matching owner/control. Client-owned mutations run on the owning client and publish a complete runtime replacement.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.SetRuntimeControlRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} config.RuntimeControlState
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.Error "Request canceled"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/config/runtime-control [put]
func (c *controllerV1) handlePutWorkspaceRuntimeControl(w http.ResponseWriter, r *http.Request) {
	c.handleWorkspaceRuntimeControl(w, r, true, true)
}

// handleDeleteWorkspaceRuntimeControl documents the workspace authority contract.
//
// @Summary Remove a declared runtime control override
// @Description Removes the exact selected override, preserving unrelated provider configuration.
// @Tags providers
// @Produce json
// @Accept json
// @Param request body proto.RuntimeControlRequest true "Exact request and operation identity"
// @Param id path string true "Workspace ID bound to the authenticated principal"
// @Success 200 {object} config.RuntimeControlState
// @Failure 400 {object} proto.Error "Invalid request; authentication operations may instead return their request-bound response with an error"
// @Failure 403 {object} proto.Error "Principal is unauthorized or does not own this workspace"
// @Failure 404 {object} proto.Error "Workspace is unavailable"
// @Failure 408 {object} proto.Error "Request canceled"
// @Failure 500 {object} proto.Error "Response unavailable; do not infer whether persistence or publication occurred"
// @Router /workspaces/{id}/config/runtime-control [delete]
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
