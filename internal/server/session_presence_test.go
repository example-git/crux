package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSessionPresenceHTTPGenerationValidation(t *testing.T) {
	c := newTestController()
	ws := installSyntheticWorkspace(t, c)
	id := uuid.NewString()
	require.NoError(t, c.backend.AttachClient(ws.ID, id))
	defer func() { _ = c.backend.RetireClient(id); c.backend.Shutdown() }()
	for _, test := range []struct {
		body     string
		status   int
		selected string
	}{
		{`{"session_id":"legacy"}`, http.StatusOK, "legacy"},
		{`{"session_id":"zero","selection_generation":0}`, http.StatusBadRequest, "legacy"},
		{`{"session_id":"negative","selection_generation":-1}`, http.StatusBadRequest, "legacy"},
		{`{"session_id":"fraction","selection_generation":1.5}`, http.StatusBadRequest, "legacy"},
		{`{"session_id":"B","selection_generation":2}`, http.StatusOK, "B"},
		{`{"session_id":"A","selection_generation":1}`, http.StatusOK, "B"},
		{`{"session_id":"B","selection_generation":2}`, http.StatusOK, "B"},
		{`{"session_id":"conflict","selection_generation":2}`, http.StatusConflict, "B"},
		{`{"session_id":"legacy-late"}`, http.StatusConflict, "B"},
		{`{"session_id":"","selection_generation":3}`, http.StatusOK, ""},
		{`{"session_id":"B","selection_generation":2}`, http.StatusOK, ""},
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/"+ws.ID+"/current-session?client_id="+id, strings.NewReader(test.body))
		request.SetPathValue("id", ws.ID)
		response := httptest.NewRecorder()
		c.handlePostWorkspaceCurrentSession(response, request)
		require.Equal(t, test.status, response.Code, test.body)
		count, err := c.backend.AttachedClients(ws.ID, test.selected)
		require.NoError(t, err)
		require.Equal(t, 1, count, test.body)
	}
}
