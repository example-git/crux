package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/proto"
)

func (c *controllerV1) handleGetWorkspacePathTranslation(w http.ResponseWriter, r *http.Request) {
	attachment, err := proto.ParseWorkspaceAttachment(r.Header)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	query := r.URL.Query()
	scope := pathtranslate.Scope{Kind: query.Get("kind"), SessionID: query.Get("session_id"), Provider: query.Get("provider"), Model: query.Get("model")}
	if _, err := scope.Key(); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	policy, err := c.backend.LoadPathTranslation(r.Context(), r.PathValue("id"), scope, attachment)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, policy)
}

func (c *controllerV1) handlePostWorkspacePathTranslation(w http.ResponseWriter, r *http.Request) {
	var policy pathtranslate.Policy
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid path translation settings")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		jsonError(w, http.StatusBadRequest, "expected one path translation policy")
		return
	}
	if _, err := policy.Scope.Key(); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := policy.Settings.Validate(); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	attachment, err := proto.ParseWorkspaceAttachment(r.Header)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := c.backend.SavePathTranslation(r.Context(), r.PathValue("id"), policy, attachment)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	jsonEncode(w, saved)
}
