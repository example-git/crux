package log

import (
	"context"
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

func TestSetupTrafficDisabled(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "unused")
	parent := context.WithValue(t.Context(), trafficContextKey{}, &networkTrace{})
	ctx, cleanup, err := SetupTraffic(parent, dataDir, false)
	require.NoError(t, err)
	defer cleanup()
	require.Nil(t, trafficFromContext(ctx))
	_, err = TrafficDatabasePath(ctx)
	require.ErrorContains(t, err, "network tracing is disabled")
	_, err = OpenTrafficDatabaseReadOnly(ctx)
	require.ErrorContains(t, err, "network tracing is disabled")
	TraceWebSocketFrame(ctx, "disabled", "inbound", "wss://example.test", 1, []byte(`{"delta":"hello"}`), nil)
	TraceWebSocketHandshake(ctx, "disabled", "outbound", "wss://example.test", nil, 0, 0, nil)
	require.NoDirExists(t, dataDir)
	require.Zero(t, testing.AllocsPerRun(100, func() {
		TraceWebSocketFrame(ctx, "disabled", "inbound", "wss://example.test", 1, nil, nil)
	}))
}

func TestSetupTrafficIsolatesWorkspacesAndInstances(t *testing.T) {
	firstDir := t.TempDir()
	paths := make(map[string]bool)
	for index, dataDir := range []string{firstDir, firstDir, t.TempDir()} {
		ctx, cleanup, err := SetupTraffic(t.Context(), dataDir, true)
		require.NoError(t, err)
		trace := trafficFromContext(ctx)
		t.Cleanup(func() {
			cleanup()
			<-trace.done
		})
		path, err := TrafficDatabasePath(ctx)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(dataDir, "traffic"), filepath.Dir(path))
		require.False(t, paths[path])
		paths[path] = true
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		requestCtx := WithTrafficContext(t.Context(), ctx)
		TraceWebSocketFrame(requestCtx, "workspace", "inbound", "wss://example.test", index+1, []byte(`{"delta":"hello"}`), nil)
		trace.flush()
		reader, err := OpenTrafficDatabaseReadOnly(requestCtx)
		require.NoError(t, err)
		events, err := QueryTraffic(requestCtx, reader, TrafficQuery{Limit: 10})
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		require.Len(t, events, 1)
		require.Equal(t, index+1, events[0].MessageType)
	}
	_, _, err := SetupTraffic(t.Context(), "", true)
	require.ErrorContains(t, err, "requires a project data directory")
}

func TestNetworkTraceFullQueueDoesNotBlockDelivery(t *testing.T) {
	trace := &networkTrace{entries: make(chan trafficWrite, 1)}
	trace.entries <- trafficWrite{}
	ctx := context.WithValue(t.Context(), trafficContextKey{}, trace)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			TraceWebSocketFrame(ctx, "full", "inbound", "wss://example.test", 1, []byte(`{"delta":"hello"}`), nil)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WebSocket trace calls blocked on a full queue")
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("delivered"))
	}))
	defer server.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	client := &http.Client{Transport: WrapHTTPTransport(server.Client().Transport), Timeout: time.Second}
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	bodyDone := make(chan string, 1)
	go func(reader io.Reader) {
		body, _ := io.ReadAll(reader)
		bodyDone <- string(body)
	}(response.Body)
	select {
	case body := <-bodyDone:
		require.Equal(t, "delivered", body)
	case <-time.After(time.Second):
		t.Fatal("HTTP delivery blocked on a full trace queue")
	}
	dropped, failed := TrafficDropCounts(ctx)
	require.EqualValues(t, 102, dropped)
	require.Zero(t, failed)
	trace.close()
	trace.close()
	TraceWebSocketFrame(ctx, "closed", "inbound", "wss://example.test", 1, nil, nil)
}

func TestNetworkTraceCountsFailedWrites(t *testing.T) {
	trace, database := testTrafficTrace(t)
	_, err := database.ExecContext(t.Context(), `DROP TABLE websocket_frames`)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), trafficContextKey{}, trace)
	TraceWebSocketFrame(ctx, "failed", "inbound", "wss://example.test", 1, nil, nil)
	trace.flush()
	dropped, failed := TrafficDropCounts(ctx)
	require.Zero(t, dropped)
	require.EqualValues(t, 1, failed)
}

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

func TestTrafficDatabaseReopenDoesNotAcquireWriterLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traffic.db")
	database, err := openTrafficDatabase(path, false)
	require.NoError(t, err)
	defer database.Close()
	transaction, err := database.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	_, err = transaction.ExecContext(t.Context(), `UPDATE traffic_meta SET value = value WHERE key = 'total_bytes'`)
	require.NoError(t, err)

	started := time.Now()
	second, err := openTrafficDatabase(path, false)
	require.NoError(t, err)
	defer second.Close()
	require.Less(t, time.Since(started), time.Second)
	require.NoError(t, transaction.Rollback())
	trace := newNetworkTrace(second)
	defer close(trace.entries)
	require.NoError(t, trace.insertBatch([]TrafficEvent{{TraceID: "reopened", Timestamp: time.Now().UTC(), Protocol: "http", Phase: "request", Direction: "outbound", Method: "GET", URL: "https://example.invalid"}}))
	var count int
	require.NoError(t, second.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_requests WHERE trace_id = 'reopened'`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestTrafficDatabaseReopenRepairsMissingSchemaObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traffic.db")
	database, err := openTrafficDatabase(path, false)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `DROP VIEW traffic_events; DELETE FROM traffic_meta WHERE key = 'total_bytes'`)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	database, err = openTrafficDatabase(path, false)
	require.NoError(t, err)
	defer database.Close()
	var count int
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM traffic_events`).Scan(&count))
	require.NoError(t, database.QueryRowContext(t.Context(), `SELECT value FROM traffic_meta WHERE key = 'total_bytes'`).Scan(&count))
	require.Zero(t, count)
}

func testTrafficTrace(t *testing.T) (*networkTrace, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "traffic.db")
	database, err := openTrafficDatabase(path, false)
	require.NoError(t, err)
	trace := newNetworkTrace(database)
	t.Cleanup(func() {
		trace.flush()
		trace.close()
		<-trace.done
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

	request, err := http.NewRequestWithContext(context.WithValue(t.Context(), trafficContextKey{}, trace), http.MethodPost, server.URL+"?access_token=secret", strings.NewReader(`{"role":"user","content":"hello"}`))
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
	require.Equal(t, events[0].ID, events[1].ID)

	selected, err := QueryTraffic(t.Context(), database, TrafficQuery{
		ID:          events[1].ID,
		Protocol:    "http",
		Phase:       "response",
		Limit:       1,
		IncludeBody: true,
	})
	require.NoError(t, err)
	require.Len(t, selected, 1)
	require.Equal(t, "response", selected[0].Phase)
	require.Contains(t, selected[0].Body, `"world"`)
	require.NotContains(t, selected[0].Body, `"hello"`)
}

func TestNetworkTraceRedactsOpaqueAccountMaterial(t *testing.T) {
	secret := "traffic-account-raw-secret-value"
	registerOpaqueAccountSecret(t, secret)
	trace, database := testTrafficTrace(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "payload "+secret, readTestBody(t, request.Body))
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	request, err := http.NewRequestWithContext(context.WithValue(t.Context(), trafficContextKey{}, trace), http.MethodPost, server.URL+"/"+secret, strings.NewReader("payload "+secret))
	require.NoError(t, err)
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
	}
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
	request, err := http.NewRequestWithContext(context.WithValue(t.Context(), trafficContextKey{}, trace), http.MethodPost, server.URL+"/"+secret, strings.NewReader("payload "+secret))
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

	request, err := http.NewRequestWithContext(context.WithValue(t.Context(), trafficContextKey{}, trace), http.MethodPost, server.URL, strings.NewReader(`{"forwarded_accounts":{"codex":{"accessToken":"secret"}}}`))
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

	request, err := http.NewRequestWithContext(context.WithValue(t.Context(), trafficContextKey{}, trace), http.MethodPost, server.URL, io.NopCloser(strings.NewReader("streamed input")))
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
	request := httptest.NewRequestWithContext(context.WithValue(t.Context(), trafficContextKey{}, trace), http.MethodPost, "http://crux.local/test", strings.NewReader("input"))
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
	TraceWebSocketHandshake(context.WithValue(t.Context(), trafficContextKey{}, trace), "test-trace", "outbound", "wss://example.test/responses", http.Header{"Authorization": {"Bearer secret"}}, 0, 0, nil)
	TraceWebSocketFrame(context.WithValue(t.Context(), trafficContextKey{}, trace), "test-trace", "outbound", "wss://example.test/responses", 1, []byte(`{"instructions":"system","input":[{"role":"user","content":"hello"}]}`), nil)
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

	request, err := http.NewRequestWithContext(context.WithValue(t.Context(), trafficContextKey{}, trace), http.MethodPost, server.URL, strings.NewReader(original))
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
	TraceWebSocketHandshake(context.WithValue(t.Context(), trafficContextKey{}, trace), "shared-trace", "outbound", "wss://example.test/responses", nil, 0, 0, nil)
	TraceWebSocketFrame(context.WithValue(t.Context(), trafficContextKey{}, trace), "shared-trace", "outbound", "wss://example.test/responses", 1, []byte(`{"input":"hello"}`), nil)
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
