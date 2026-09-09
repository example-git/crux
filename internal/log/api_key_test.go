package log

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyCheckDebugLoggerDoesNotInspectPayload(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, `{"source":"synthetic-arbitrary-private-value"}`, string(body))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/workspaces/fixture/auth/api-key/check", strings.NewReader(`{"source":"synthetic-arbitrary-private-value"}`))
	require.NoError(t, err)
	response, err := (&http.Client{Transport: &HTTPRoundTripLogger{Transport: server.Client().Transport}}).Do(request)
	require.NoError(t, err)
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Contains(t, string(data), "synthetic-arbitrary-private-value")
	require.NotContains(t, output.String(), "synthetic-arbitrary-private-value")
}
