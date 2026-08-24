package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskHandlersRejectInvalidExplicitInputs(t *testing.T) {
	controller := &controllerV1{}

	response := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/workspace/tasks/b12345678/output", strings.NewReader(`{"wait":true,"timeout":600000000001}`))
	controller.handlePostWorkspaceTaskOutput(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "between 0 and 10 minutes")

	response = httptest.NewRecorder()
	request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/workspace/tasks/a12345678/continue", strings.NewReader(`{"parent_session_id":"","prompt":"continue"}`))
	controller.handlePostWorkspaceTaskContinue(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "parent_session_id and prompt are required")

	response = httptest.NewRecorder()
	request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/workspaces/workspace/task-notifications?unread_only=invalid", nil)
	controller.handleGetWorkspaceTaskNotifications(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "unread_only must be a boolean")
}
