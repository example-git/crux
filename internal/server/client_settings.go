package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/config"
)

func (c *controllerV1) handlePostClientSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if c.server.remoteManagement() || c.server.tlsConfig != nil {
		jsonError(w, http.StatusForbidden, config.ErrClientRuntimeManaged.Error())
		return
	}
	var request config.ClientSettingsWriteRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, config.MaxClientSettingsBytes))
	if decoder.Decode(&request) != nil {
		jsonError(w, http.StatusBadRequest, "invalid client settings request")
		return
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		jsonError(w, http.StatusBadRequest, "client settings request must contain one JSON object")
		return
	}
	if err := request.Validate(); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := c.backend.WriteClientSettings(r.Context(), request)
	if err != nil {
		switch {
		case errors.Is(err, config.ErrClientSettingsValidation):
			jsonError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, config.ErrClientSettingsVersionConflict):
			jsonError(w, http.StatusConflict, err.Error())
		case errors.Is(err, config.ErrClientSettingsReadonly), errors.Is(err, config.ErrClientRuntimeManaged):
			jsonError(w, http.StatusForbidden, err.Error())
		default:
			c.handleError(w, r, err)
		}
		return
	}
	jsonEncode(w, result)
}
