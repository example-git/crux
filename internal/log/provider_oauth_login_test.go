package log

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderOAuthRoutesSuppressBothPayloadDirectionsForEveryMethod(t *testing.T) {
	for _, action := range []string{"oauth/begin", "oauth/bind", "oauth/code", "oauth/wait", "oauth/cancel", "oauth/complete", "remove"} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			t.Run(action+"/"+method, func(t *testing.T) {
				trace, database := testTrafficTrace(t)
				var output bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
				t.Cleanup(func() { slog.SetDefault(previous) })
				const secret = "synthetic-unregistered-private-value"
				handler := TraceHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Contains(t, string(body), secret)
					w.WriteHeader(400)
					_, _ = io.WriteString(w, "malformed response with "+secret)
				}))
				host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), trafficContextKey{}, trace)))
				}))
				defer host.Close()
				ctx := context.WithValue(t.Context(), trafficContextKey{}, trace)
				request, err := http.NewRequestWithContext(ctx, method, host.URL+"/v1/workspaces/workspace/auth/"+action, strings.NewReader("malformed input with "+secret))
				require.NoError(t, err)
				transport := &HTTPRoundTripLogger{Transport: WrapHTTPTransport(host.Client().Transport)}
				response, err := (&http.Client{Transport: transport}).Do(request)
				require.NoError(t, err)
				body, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Contains(t, string(body), secret, "privacy must not replace the actual HTTP payload")
				require.NoError(t, response.Body.Close())
				trace.flush()
				for _, table := range []string{"http_requests", "http_responses"} {
					var count int
					require.NoError(t, database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
					require.Positive(t, count, "the actual exchange must retain safe tracing metadata")
				}
				for _, table := range []string{"http_request_payloads", "http_response_payloads"} {
					var count int
					require.NoError(t, database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
					require.Zero(t, count)
				}
				require.NotContains(t, output.String(), secret)
			})
		}
	}
}
