package server

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCopilotImportRejectsMalformedRequestsBeforeBackend(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"owner":null}`, `{"owner":{}}`,
		`{"owner":{"provider_id":"other"}}`,
		`{"owner":{"provider_id":"copilot"},"owner":{}}`,
		`{"owner":{"provider_id":"copilot"},"secret":"synthetic-private"}`,
		`{} {}`, strings.Repeat("[", 66) + strings.Repeat("]", 66),
		`{"owner":{"provider_id":"` + strings.Repeat("x", 16<<10) + `"}}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/workspaces/fixture/import-copilot", strings.NewReader(body))
		response := httptest.NewRecorder()
		// A nil backend makes accidental dispatch observable as a panic.
		(&controllerV1{}).handlePostWorkspaceConfigImportCopilot(response, r)
		require.Equal(t, http.StatusBadRequest, response.Code)
		require.NotContains(t, response.Body.String(), "synthetic-private")
	}
}
