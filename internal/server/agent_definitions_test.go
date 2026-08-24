package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/agent"
	"github.com/stretchr/testify/require"
)

func TestHandleErrorMapsAgentDefinitionErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{name: "invalid", err: errors.Join(agent.ErrInvalidAgentDefinition, errors.New("invalid scope")), status: http.StatusBadRequest},
		{name: "exists", err: errors.Join(agent.ErrAgentDefinitionExists, errors.New("already exists")), status: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &controllerV1{server: &Server{}}
			response := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/ws1/agent/definitions", nil)
			controller.handleError(response, request, test.err)
			require.Equal(t, test.status, response.Code)
			require.Equal(t, "application/json", response.Header().Get("Content-Type"))
		})
	}
}

func TestPostWorkspaceAgentDefinitionRejectsUnknownFields(t *testing.T) {
	controller := &controllerV1{server: &Server{}}
	response := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/ws1/agent/definitions", strings.NewReader(`{"unknown":true}`))
	controller.handlePostWorkspaceAgentDefinition(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)
}
