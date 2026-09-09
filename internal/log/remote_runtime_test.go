package log

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrivateRuntimeRoutesNeverPersistBodiesWithoutMarker(t *testing.T) {
	for _, test := range []struct{ method, path string }{{http.MethodPost, "/v1/workspaces"}, {http.MethodPut, "/v1/workspaces/workspace/runtime"}, {http.MethodPost, "/v1/workspaces/workspace/auth/api-key/check"}, {http.MethodGet, "/v1/workspaces/workspace/auth/api-key/check"}} {
		t.Run(test.method+test.path, func(t *testing.T) {
			trace, database := testTrafficTrace(t)
			handler := TraceHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.Copy(io.Discard, r.Body)
				require.NoError(t, err)
				w.WriteHeader(http.StatusBadRequest)
				if strings.HasSuffix(test.path, "/auth/api-key/check") {
					_, _ = io.WriteString(w, `{"source":"synthetic-private-unregistered-value"}`)
				}
			}))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), trafficContextKey{}, trace)))
			}))
			defer server.Close()
			request, err := http.NewRequestWithContext(context.WithValue(t.Context(), trafficContextKey{}, trace), test.method, server.URL+test.path, strings.NewReader(`{"private_input":"synthetic-private-unregistered-value"}`))
			require.NoError(t, err)
			response, err := (&http.Client{Transport: WrapHTTPTransport(server.Client().Transport)}).Do(request)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			trace.flush()
			var count int
			require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_request_payloads`).Scan(&count))
			require.Zero(t, count)
			if strings.HasSuffix(test.path, "/auth/api-key/check") {
				require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_response_payloads`).Scan(&count))
				require.Zero(t, count)
			}
			events, err := QueryTraffic(t.Context(), database, TrafficQuery{Limit: 20, IncludeBody: true})
			require.NoError(t, err)
			for _, event := range events {
				require.NotContains(t, event.Body, "synthetic-private")
			}
		})
	}
}
