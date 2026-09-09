package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/proto"
)

func (c *controllerV1) handlePutWorkspaceProviderTooling(w http.ResponseWriter, r *http.Request) {
	c.handleWorkspaceProviderTooling(w, r, false)
}

func (c *controllerV1) handleDeleteWorkspaceProviderTooling(w http.ResponseWriter, r *http.Request) {
	c.handleWorkspaceProviderTooling(w, r, true)
}

func (c *controllerV1) handleWorkspaceProviderTooling(w http.ResponseWriter, r *http.Request, remove bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil || validateRuntimeJSON(body) != nil {
		jsonError(w, http.StatusBadRequest, "invalid provider tooling request")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request proto.ProviderToolingRequest
	if remove {
		var removal proto.RemoveProviderToolingRequest
		err = decoder.Decode(&removal)
		request.Scope, request.Owner = removal.Scope, removal.Owner
	} else {
		err = decoder.Decode(&request)
	}
	if err != nil || decoder.Decode(new(any)) != io.EOF || proto.ValidateProviderToolingRequest(request.Scope, request.Owner, request.Profile, remove) != nil {
		jsonError(w, http.StatusBadRequest, "invalid provider tooling request")
		return
	}
	var state proto.ProviderToolingState
	if remove {
		state, err = c.backend.RemoveProviderToolingInstructions(r.Context(), r.PathValue("id"), *request.Scope, request.Owner)
	} else {
		state, err = c.backend.SetProviderToolingInstructions(r.Context(), r.PathValue("id"), *request.Scope, request.Owner, request.Profile)
	}
	if err != nil {
		switch {
		case errors.Is(err, backend.ErrWorkspaceNotFound), errors.Is(err, backend.ErrWorkspaceClosing):
			c.handleError(w, r, err)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			jsonError(w, http.StatusRequestTimeout, "provider tooling request canceled")
		default:
			jsonError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	jsonEncode(w, state)
}
