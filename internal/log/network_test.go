package log

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func TestTrafficDatabaseFileURL(t *testing.T) {
	testCases := map[string]struct {
		path string
		want string
	}{
		"unix": {
			path: "/tmp/traffic log.db",
			want: "file:///tmp/traffic%20log.db",
		},
		"windows drive": {
			path: `C:\Users\runner\traffic.db`,
			want: "file:///C:/Users/runner/traffic.db",
		},
		"windows UNC": {
			path: `\\server\share\traffic.db`,
			want: "file:////server/share/traffic.db",
		},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, testCase.want, trafficDatabaseFileURL(testCase.path))
		})
	}
}

func testTrafficTrace(t *testing.T) (*networkTrace, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "traffic.db")
	database, err := openTrafficDatabase(path, false)
	require.NoError(t, err)
	trace := newNetworkTrace(database)
	previous := networkTraceState.Swap(trace)
	t.Cleanup(func() {
		trace.flush()
		networkTraceState.Store(previous)
		require.NoError(t, database.Close())
	})
	return trace, database
}

func TestNetworkTraceCapturesOutboundHTTPInTypedTables(t *testing.T) {
	trace, database := testTrafficTrace(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, `{"role":"user","content":"hello"}`, readTestBody(t, request.Body))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"role":"assistant","content":"world"}`))
	}))
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"?access_token=secret", strings.NewReader(`{"role":"user","content":"hello"}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer secret")
	client := &http.Client{Transport: WrapHTTPTransport(server.Client().Transport)}
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, `{"role":"assistant","content":"world"}`, readTestBody(t, response.Body))
	require.NoError(t, response.Body.Close())
	trace.flush()

	var requests, requestPayloads, responses, responsePayloads int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_requests`).Scan(&requests))
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_request_payloads`).Scan(&requestPayloads))
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_responses`).Scan(&responses))
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_response_payloads`).Scan(&responsePayloads))
	require.Equal(t, 1, requests)
	require.Equal(t, 1, requestPayloads)
	require.Equal(t, 1, responses)
	require.Equal(t, 1, responsePayloads)

	events, err := QueryTraffic(t.Context(), database, TrafficQuery{Sort: "asc", Limit: 10, IncludeBody: true})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "[REDACTED]", http.Header(events[0].Headers).Get("Authorization"))
	require.Contains(t, events[0].URL, "access_token=%5BREDACTED%5D")
	require.Contains(t, events[0].Body, `"hello"`)
	require.Contains(t, events[1].Body, `"world"`)
	require.Equal(t, os.Getpid(), events[0].ProcessID)
}

func TestNetworkTraceRedactsCopiesWithoutChangingProviderRequest(t *testing.T) {
	secret := "provider-request-secret-value"
	redact.Register(secret)
	trace, database := testTrafficTrace(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, secret, request.Header.Get("X-Custom-Credential"))
		require.Equal(t, "payload "+secret, readTestBody(t, request.Body))
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/"+secret, strings.NewReader("payload "+secret))
	require.NoError(t, err)
	request.Header.Set("X-Custom-Credential", secret)
	response, err := (&http.Client{Transport: WrapHTTPTransport(server.Client().Transport)}).Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	trace.flush()
	events, err := QueryTraffic(t.Context(), database, TrafficQuery{Sort: "asc", Limit: 10, IncludeBody: true})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	for _, event := range events {
		require.NotContains(t, event.URL, secret)
		require.NotContains(t, event.Body, secret)
		for _, values := range event.Headers {
			require.NotContains(t, strings.Join(values, " "), secret)
		}
	}
}

func TestNetworkTraceDoesNotPersistEphemeralStateBodies(t *testing.T) {
	trace, database := testTrafficTrace(t)
	server := httptest.NewServer(TraceHTTPHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, `{"forwarded_accounts":{"codex":{"accessToken":"secret"}}}`, readTestBody(t, request.Body))
		writer.WriteHeader(http.StatusNoContent)
	})))
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(`{"forwarded_accounts":{"codex":{"accessToken":"secret"}}}`))
	require.NoError(t, err)
	request.Header.Set(EphemeralStateHeader, "1")
	response, err := (&http.Client{Transport: WrapHTTPTransport(server.Client().Transport)}).Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	trace.flush()

	var requestPayloads int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_request_payloads`).Scan(&requestPayloads))
	require.Zero(t, requestPayloads)
	events, err := QueryTraffic(t.Context(), database, TrafficQuery{Sort: "asc", Limit: 10, IncludeBody: true})
	require.NoError(t, err)
	for _, event := range events {
		require.NotContains(t, event.Body, "secret")
	}
}

func TestNetworkTraceStreamsRequestBodiesWithoutGetBody(t *testing.T) {
	trace, database := testTrafficTrace(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "streamed input", readTestBody(t, request.Body))
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, io.NopCloser(strings.NewReader("streamed input")))
	require.NoError(t, err)
	require.Nil(t, request.GetBody)
	response, err := (&http.Client{Transport: WrapHTTPTransport(server.Client().Transport)}).Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	trace.flush()

	events, err := QueryTraffic(t.Context(), database, TrafficQuery{Sort: "asc", Limit: 10, IncludeBody: true})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "streamed input", events[0].Body)
	require.Equal(t, events[0].TraceID, events[1].TraceID)
}

func TestNetworkTraceCapturesInboundHTTP(t *testing.T) {
	trace, database := testTrafficTrace(t)
	handler := TraceHTTPHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "input", readTestBody(t, request.Body))
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte("output"))
	}))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://crux.local/test", strings.NewReader("input"))
	request.Header.Set("Cookie", "session=secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	trace.flush()

	events, err := QueryTraffic(t.Context(), database, TrafficQuery{Sort: "asc", Limit: 10, IncludeBody: true})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "inbound", events[0].Direction)
	require.Equal(t, "input", events[0].Body)
	require.Equal(t, "[REDACTED]", http.Header(events[0].Headers).Get("Cookie"))
	require.Equal(t, "outbound", events[1].Direction)
	require.Equal(t, http.StatusCreated, events[1].StatusCode)
	require.Equal(t, "output", events[1].Body)
}

func TestNetworkTraceStoresWebSocketTypesSeparately(t *testing.T) {
	trace, database := testTrafficTrace(t)
	TraceWebSocketHandshake("test-trace", "outbound", "wss://example.test/responses", http.Header{"Authorization": {"Bearer secret"}}, 0, 0, nil)
	TraceWebSocketFrame("test-trace", "outbound", "wss://example.test/responses", 1, []byte(`{"instructions":"system","input":[{"role":"user","content":"hello"}]}`), nil)
	trace.flush()

	var handshakes, frames, payloads int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM websocket_handshakes`).Scan(&handshakes))
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM websocket_frames`).Scan(&frames))
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM websocket_frame_payloads`).Scan(&payloads))
	require.Equal(t, 1, handshakes)
	require.Equal(t, 1, frames)
	require.Equal(t, 1, payloads)
}

func TestNetworkTraceSupportsConcurrentWritersAndReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traffic.db")
	firstDatabase, err := openTrafficDatabase(path, false)
	require.NoError(t, err)
	defer firstDatabase.Close()
	secondDatabase, err := openTrafficDatabase(path, false)
	require.NoError(t, err)
	defer secondDatabase.Close()
	reader, err := openTrafficDatabase(path, true)
	require.NoError(t, err)
	defer reader.Close()
	first := newNetworkTrace(firstDatabase)
	second := newNetworkTrace(secondDatabase)

	var journalMode string
	require.NoError(t, firstDatabase.QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&journalMode))
	require.Equal(t, "wal", strings.ToLower(journalMode))

	var writers sync.WaitGroup
	writers.Add(2)
	start := make(chan struct{})
	for index, trace := range []*networkTrace{first, second} {
		go func(index int, trace *networkTrace) {
			defer writers.Done()
			<-start
			for sequence := range 100 {
				trace.record(TrafficEvent{TraceID: nextNetworkTraceID(), Protocol: "http", Direction: "outbound", Phase: "request", Method: http.MethodGet, URL: "https://example.test/" + string(rune('a'+index)), Body: strings.Repeat("x", sequence%5)})
			}
		}(index, trace)
	}
	readerErrors := make(chan error, 4)
	var readers sync.WaitGroup
	readers.Add(4)
	for range 4 {
		go func() {
			defer readers.Done()
			<-start
			for range 20 {
				_, queryErr := QueryTraffic(t.Context(), reader, TrafficQuery{Limit: 10})
				if queryErr != nil {
					readerErrors <- queryErr
					return
				}
			}
		}()
	}
	close(start)
	writers.Wait()
	readers.Wait()
	close(readerErrors)
	for readerErr := range readerErrors {
		require.NoError(t, readerErr)
	}
	first.flush()
	second.flush()

	events, err := QueryTraffic(t.Context(), reader, TrafficQuery{Limit: 250})
	require.NoError(t, err)
	require.Len(t, events, 200)
}

func TestTrafficRetentionDeletesOldestTypedRecord(t *testing.T) {
	_, database := testTrafficTrace(t)
	transaction, err := database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	for index, table := range []string{"http_requests", "websocket_frames"} {
		if table == "http_requests" {
			_, err = transaction.ExecContext(t.Context(), `INSERT INTO http_requests(timestamp_ns, timestamp, process_id, trace_id, direction, method, url, size_bytes) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, index+1, time.Now().Format(time.RFC3339Nano), os.Getpid(), "trace", "outbound", "GET", "https://example.test", trafficRetentionBytes/2+1)
		} else {
			_, err = transaction.ExecContext(t.Context(), `INSERT INTO websocket_frames(timestamp_ns, timestamp, process_id, trace_id, direction, url, size_bytes) VALUES (?, ?, ?, ?, ?, ?, ?)`, index+1, time.Now().Format(time.RFC3339Nano), os.Getpid(), "trace", "outbound", "wss://example.test", trafficRetentionBytes/2+1)
		}
		require.NoError(t, err)
	}
	_, err = transaction.ExecContext(t.Context(), `UPDATE traffic_meta SET value = ? WHERE key = 'total_bytes'`, trafficRetentionBytes+2)
	require.NoError(t, err)
	require.NoError(t, enforceTrafficRetention(transaction))
	require.NoError(t, transaction.Commit())

	var requests, frames int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_requests`).Scan(&requests))
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM websocket_frames`).Scan(&frames))
	require.Zero(t, requests)
	require.Equal(t, 1, frames)
}

func TestQueryTrafficSearchSortAndBodyLimit(t *testing.T) {
	trace, database := testTrafficTrace(t)
	trace.record(TrafficEvent{TraceID: "first", Protocol: "websocket", Direction: "outbound", Phase: "frame", URL: "wss://example.test", Body: `{"instructions":"alpha"}`})
	time.Sleep(time.Millisecond)
	trace.record(TrafficEvent{TraceID: "second", Protocol: "websocket", Direction: "inbound", Phase: "frame", URL: "wss://example.test", Body: `{"delta":"beta"}`})
	trace.flush()

	events, err := QueryTraffic(t.Context(), database, TrafficQuery{Search: "alpha", Protocol: "websocket", Sort: "asc", Limit: 10, IncludeBody: true, BodyLimit: 8})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "first", events[0].TraceID)
	require.Contains(t, events[0].Body, "truncated")
}

func TestNetworkTraceRedactsCredentialFieldsWithoutChangingRequest(t *testing.T) {
	trace, database := testTrafficTrace(t)
	original := `{"instructions":"keep this","api_key":"secret","nested":{"access_token":"token","content":"message"}}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, original, readTestBody(t, request.Body))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"refresh_token":"response-secret","content":"answer"}`))
	}))
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(original))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: WrapHTTPTransport(server.Client().Transport)}).Do(request)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	trace.flush()

	events, err := QueryTraffic(t.Context(), database, TrafficQuery{Sort: "asc", Limit: 10, IncludeBody: true})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.NotContains(t, events[0].Body, "secret")
	require.Contains(t, events[0].Body, `"api_key":"[REDACTED]"`)
	require.Contains(t, events[0].Body, `"instructions":"keep this"`)
	require.NotContains(t, events[1].Body, "response-secret")
	require.Contains(t, events[1].Body, `"content":"answer"`)
}

func TestTrafficCaptureBoundsStoredPayload(t *testing.T) {
	var capture trafficCapture
	data := []byte(strings.Repeat("x", trafficMaxPayloadBytes+1024))
	n, err := capture.Write(data)
	require.NoError(t, err)
	require.Equal(t, len(data), n)
	require.Equal(t, int64(len(data)), capture.total)
	require.Len(t, capture.Bytes(), trafficMaxPayloadBytes)
	body, encoding := capture.Encode("text/plain")
	require.Len(t, body, trafficMaxPayloadBytes)
	require.Equal(t, "utf-8-truncated", encoding)
}

func TestNetworkResponseWriterKeepsFirstStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &networkResponseWriter{ResponseWriter: recorder, statusCode: http.StatusOK}
	writer.WriteHeader(http.StatusCreated)
	writer.WriteHeader(http.StatusInternalServerError)
	_, err := writer.Write([]byte("body"))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, writer.statusCode)
	require.Equal(t, http.StatusCreated, recorder.Code)
	require.Equal(t, int64(4), writer.body.total)
}

func TestWebSocketEventsShareCallerTraceID(t *testing.T) {
	trace, database := testTrafficTrace(t)
	TraceWebSocketHandshake("shared-trace", "outbound", "wss://example.test/responses", nil, 0, 0, nil)
	TraceWebSocketFrame("shared-trace", "outbound", "wss://example.test/responses", 1, []byte(`{"input":"hello"}`), nil)
	trace.flush()

	events, err := QueryTraffic(t.Context(), database, TrafficQuery{Sort: "asc", Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "shared-trace", events[0].TraceID)
	require.Equal(t, "shared-trace", events[1].TraceID)
}

func readTestBody(t *testing.T, body io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	return string(data)
}
