package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderUsageRejectsMalformedRequestsBeforeBackend(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"owner":null}`, `{"owner":{},"revision":-1}`,
		`{"owner":{},"revision":1,"revision":2}`,
		`{"owner":{},"secret":"synthetic-private"}`,
		`{} {}`, strings.Repeat("[", 66) + strings.Repeat("]", 66),
		`{"digest":"` + strings.Repeat("x", 16<<10) + `"}`,
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/fixture/providers/usage", strings.NewReader(body))
		response := httptest.NewRecorder()
		// A nil backend makes an accidental dispatch observable as a panic.
		(&controllerV1{}).handlePostWorkspaceProviderUsage(response, r)
		require.Equal(t, http.StatusBadRequest, response.Code)
		require.NotContains(t, response.Body.String(), "synthetic-private")
	}
}
