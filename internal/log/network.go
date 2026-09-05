package log

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/example-git/crux/internal/redact"
	_ "modernc.org/sqlite"
)

const (
	trafficRetentionBytes  = 40 * 1024 * 1024
	trafficQueueSize       = 1024
	trafficBatchSize       = 64
	trafficBatchDelay      = 20 * time.Millisecond
	trafficBusyRetries     = 6
	trafficMaxPayloadBytes = 16 * 1024 * 1024
)

var (
	networkTraceState atomic.Pointer[networkTrace]
	networkTraceID    atomic.Uint64
	defaultHTTPBase   = http.DefaultTransport.(*http.Transport).Clone()
	defaultTraceOnce  sync.Once
	networkSetupOnce  sync.Once
	networkSetupErr   error
)

type networkTrace struct {
	database *sql.DB
	entries  chan trafficWrite
}

type trafficWrite struct {
	event *TrafficEvent
	done  chan struct{}
}

type TrafficEvent struct {
	ID            int64               `json:"id,omitempty"`
	Timestamp     time.Time           `json:"timestamp"`
	ProcessID     int                 `json:"process_id"`
	TraceID       string              `json:"trace_id,omitempty"`
	Protocol      string              `json:"protocol"`
	Direction     string              `json:"direction"`
	Phase         string              `json:"phase"`
	Method        string              `json:"method,omitempty"`
	URL           string              `json:"url,omitempty"`
	StatusCode    int                 `json:"status_code,omitempty"`
	Headers       map[string][]string `json:"headers,omitempty"`
	Body          string              `json:"body,omitempty"`
	BodyEncoding  string              `json:"body_encoding,omitempty"`
	ContentLength int64               `json:"content_length,omitempty"`
	DurationMS    int64               `json:"duration_ms,omitempty"`
	MessageType   int                 `json:"message_type,omitempty"`
	Error         string              `json:"error,omitempty"`
}

type TrafficQuery struct {
	ID          int64
	Search      string
	Protocol    string
	Direction   string
	Phase       string
	Since       time.Time
	Until       time.Time
	Sort        string
	Limit       int
	BodyLimit   int
	IncludeBody bool
}

func TrafficDatabasePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".ai-cli", "traffic", "crux.db"), nil
}

func SetupTraffic() error {
	networkSetupOnce.Do(func() {
		path, err := TrafficDatabasePath()
		if err != nil {
			networkSetupErr = err
			return
		}
		database, err := openTrafficDatabase(path, false)
		if err != nil {
			networkSetupErr = err
			return
		}
		trace := newNetworkTrace(database)
		networkTraceState.Store(trace)
		defaultTraceOnce.Do(func() {
			http.DefaultTransport = WrapHTTPTransport(defaultHTTPBase)
		})
	})
	return networkSetupErr
}

func trafficDatabaseFileURL(path string) string {
	normalized := filepath.ToSlash(path)
	if len(normalized) >= 3 && normalized[1] == ':' && (normalized[2] == '/' || normalized[2] == '\\') {
		normalized = "/" + strings.ReplaceAll(normalized, "\\", "/")
	} else if strings.HasPrefix(normalized, `\\`) {
		normalized = strings.ReplaceAll(normalized, "\\", "/")
	}
	return (&url.URL{Scheme: "file", Path: normalized}).String()
}

func openTrafficDatabase(path string, readOnly bool) (*sql.DB, error) {
	if !readOnly {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create traffic directory: %w", err)
		}
	}
	mode := "rwc"
	if readOnly {
		mode = "ro"
	}
	fileURL := trafficDatabaseFileURL(path)
	dsn := fileURL + "?mode=" + mode + "&_pragma=busy_timeout(5000)"
	if !readOnly {
		dsn += "&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=journal_size_limit(10485760)&_pragma=wal_autocheckpoint(1000)"
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open traffic database: %w", err)
	}
	if readOnly {
		database.SetMaxOpenConns(4)
	} else {
		database.SetMaxOpenConns(1)
	}
	if err := database.PingContext(context.Background()); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("connect traffic database: %w", err)
	}
	if readOnly {
		return database, nil
	}
	if err := initializeTrafficDatabase(database); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("secure traffic database: %w", err)
	}
	return database, nil
}

func initializeTrafficDatabase(database *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS http_requests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp_ns INTEGER NOT NULL,
			timestamp TEXT NOT NULL,
			process_id INTEGER NOT NULL,
			trace_id TEXT NOT NULL,
			direction TEXT NOT NULL,
			method TEXT NOT NULL,
			url TEXT NOT NULL,
			headers_json TEXT NOT NULL DEFAULT '{}',
			content_length INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			size_bytes INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS http_request_payloads (
			request_id INTEGER PRIMARY KEY REFERENCES http_requests(id) ON DELETE CASCADE,
			body BLOB NOT NULL,
			body_encoding TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS http_responses (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp_ns INTEGER NOT NULL,
			timestamp TEXT NOT NULL,
			process_id INTEGER NOT NULL,
			trace_id TEXT NOT NULL,
			direction TEXT NOT NULL,
			method TEXT NOT NULL,
			url TEXT NOT NULL,
			status_code INTEGER NOT NULL DEFAULT 0,
			headers_json TEXT NOT NULL DEFAULT '{}',
			content_length INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			size_bytes INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS http_response_payloads (
			response_id INTEGER PRIMARY KEY REFERENCES http_responses(id) ON DELETE CASCADE,
			body BLOB NOT NULL,
			body_encoding TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS websocket_handshakes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp_ns INTEGER NOT NULL,
			timestamp TEXT NOT NULL,
			process_id INTEGER NOT NULL,
			trace_id TEXT NOT NULL,
			direction TEXT NOT NULL,
			url TEXT NOT NULL,
			status_code INTEGER NOT NULL DEFAULT 0,
			headers_json TEXT NOT NULL DEFAULT '{}',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			size_bytes INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS websocket_frames (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp_ns INTEGER NOT NULL,
			timestamp TEXT NOT NULL,
			process_id INTEGER NOT NULL,
			trace_id TEXT NOT NULL,
			direction TEXT NOT NULL,
			url TEXT NOT NULL,
			message_type INTEGER NOT NULL DEFAULT 0,
			content_length INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			size_bytes INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS websocket_frame_payloads (
			frame_id INTEGER PRIMARY KEY REFERENCES websocket_frames(id) ON DELETE CASCADE,
			body BLOB NOT NULL,
			body_encoding TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS http_requests_timestamp_idx ON http_requests(timestamp_ns, id)`,
		`CREATE INDEX IF NOT EXISTS http_requests_trace_idx ON http_requests(trace_id)`,
		`CREATE INDEX IF NOT EXISTS http_responses_timestamp_idx ON http_responses(timestamp_ns, id)`,
		`CREATE INDEX IF NOT EXISTS http_responses_trace_idx ON http_responses(trace_id)`,
		`CREATE INDEX IF NOT EXISTS websocket_handshakes_timestamp_idx ON websocket_handshakes(timestamp_ns, id)`,
		`CREATE INDEX IF NOT EXISTS websocket_frames_timestamp_idx ON websocket_frames(timestamp_ns, id)`,
		`CREATE TABLE IF NOT EXISTS traffic_meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO traffic_meta(key, value) VALUES ('total_bytes', 0)`,
		`UPDATE traffic_meta SET value =
			COALESCE((SELECT SUM(size_bytes) FROM http_requests), 0) +
			COALESCE((SELECT SUM(size_bytes) FROM http_responses), 0) +
			COALESCE((SELECT SUM(size_bytes) FROM websocket_handshakes), 0) +
			COALESCE((SELECT SUM(size_bytes) FROM websocket_frames), 0)
			WHERE key = 'total_bytes'`,
		`DROP VIEW IF EXISTS traffic_events`,
		`CREATE VIEW traffic_events AS
			SELECT 'http_request' AS storage_type, r.id, r.timestamp_ns, r.timestamp,
				r.process_id, r.trace_id, 'http' AS protocol, r.direction,
				'request' AS phase, r.method, r.url, 0 AS status_code,
				r.headers_json, COALESCE(p.body, X'') AS body,
				COALESCE(p.body_encoding, '') AS body_encoding, r.content_length,
				0 AS duration_ms, 0 AS message_type, r.error
			FROM http_requests r LEFT JOIN http_request_payloads p ON p.request_id = r.id
			UNION ALL
			SELECT 'http_response', r.id, r.timestamp_ns, r.timestamp,
				r.process_id, r.trace_id, 'http', r.direction,
				'response', r.method, r.url, r.status_code,
				r.headers_json, COALESCE(p.body, X''), COALESCE(p.body_encoding, ''),
				r.content_length, r.duration_ms, 0, r.error
			FROM http_responses r LEFT JOIN http_response_payloads p ON p.response_id = r.id
			UNION ALL
			SELECT 'websocket_handshake', h.id, h.timestamp_ns, h.timestamp,
				h.process_id, h.trace_id, 'websocket', h.direction,
				'handshake', '', h.url, h.status_code, h.headers_json, X'', '',
				0, h.duration_ms, 0, h.error
			FROM websocket_handshakes h
			UNION ALL
			SELECT 'websocket_frame', f.id, f.timestamp_ns, f.timestamp,
				f.process_id, f.trace_id, 'websocket', f.direction,
				'frame', '', f.url, 0, '{}', COALESCE(p.body, X''),
				COALESCE(p.body_encoding, ''), f.content_length, 0,
				f.message_type, f.error
			FROM websocket_frames f LEFT JOIN websocket_frame_payloads p ON p.frame_id = f.id`,
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(context.Background(), statement); err != nil {
			return fmt.Errorf("initialize traffic database: %w", err)
		}
	}
	return nil
}

func OpenTrafficDatabaseReadOnly() (*sql.DB, error) {
	path, err := TrafficDatabasePath()
	if err != nil {
		return nil, err
	}
	return openTrafficDatabase(path, true)
}

func newNetworkTrace(database *sql.DB) *networkTrace {
	trace := &networkTrace{database: database, entries: make(chan trafficWrite, trafficQueueSize)}
	go func() {
		batch := make([]TrafficEvent, 0, trafficBatchSize)
		timer := time.NewTimer(trafficBatchDelay)
		if !timer.Stop() {
			<-timer.C
		}
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := trace.insertBatch(batch); err != nil {
				slog.Error("Failed to write traffic batch", "error", err, "events", len(batch))
			}
			batch = batch[:0]
		}
		for {
			var timeout <-chan time.Time
			if len(batch) > 0 {
				timeout = timer.C
			}
			select {
			case write, ok := <-trace.entries:
				if !ok {
					flush()
					return
				}
				if write.event != nil {
					if len(batch) == 0 {
						timer.Reset(trafficBatchDelay)
					}
					batch = append(batch, *write.event)
					if len(batch) >= trafficBatchSize {
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
						flush()
					}
				}
				if write.done != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					flush()
					close(write.done)
				}
			case <-timeout:
				flush()
			}
		}
	}()
	return trace
}

func (t *networkTrace) record(event TrafficEvent) {
	if t == nil {
		return
	}
	event.URL = redact.String(event.URL)
	event.Headers = formatHeaders(event.Headers)
	event.Body = redact.String(event.Body)
	event.Error = redact.String(event.Error)
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	event.ProcessID = os.Getpid()
	t.entries <- trafficWrite{event: &event}
}

func (t *networkTrace) flush() {
	if t == nil {
		return
	}
	done := make(chan struct{})
	t.entries <- trafficWrite{done: done}
	<-done
}

func (t *networkTrace) insertBatch(events []TrafficEvent) error {
	var lastErr error
	for attempt := 0; attempt < trafficBusyRetries; attempt++ {
		lastErr = t.insertBatchOnce(events)
		if lastErr == nil || !isTrafficBusy(lastErr) {
			return lastErr
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return lastErr
}

func (t *networkTrace) insertBatchOnce(events []TrafficEvent) error {
	transaction, err := t.database.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	var addedBytes int64
	for _, event := range events {
		size, err := insertTrafficEvent(transaction, event)
		if err != nil {
			return err
		}
		addedBytes += size
	}
	if _, err := transaction.ExecContext(context.Background(), `UPDATE traffic_meta SET value = value + ? WHERE key = 'total_bytes'`, addedBytes); err != nil {
		return err
	}
	if err := enforceTrafficRetention(transaction); err != nil {
		return err
	}
	return transaction.Commit()
}

func insertTrafficEvent(transaction *sql.Tx, event TrafficEvent) (int64, error) {
	headers, err := json.Marshal(event.Headers)
	if err != nil {
		return 0, err
	}
	size := int64(len(headers) + len(event.Body) + len(event.TraceID) + len(event.Protocol) + len(event.Direction) + len(event.Phase) + len(event.Method) + len(event.URL) + len(event.Error) + 128)
	timestamp := event.Timestamp.Format(time.RFC3339Nano)
	var result sql.Result
	switch {
	case event.Protocol == "http" && event.Phase == "request":
		result, err = transaction.ExecContext(context.Background(), `INSERT INTO http_requests (
			timestamp_ns, timestamp, process_id, trace_id, direction, method, url,
			headers_json, content_length, error, size_bytes
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			event.Timestamp.UnixNano(), timestamp, event.ProcessID, event.TraceID,
			event.Direction, event.Method, event.URL, string(headers),
			event.ContentLength, event.Error, size)
	case event.Protocol == "http" && event.Phase == "response":
		result, err = transaction.ExecContext(context.Background(), `INSERT INTO http_responses (
			timestamp_ns, timestamp, process_id, trace_id, direction, method, url,
			status_code, headers_json, content_length, duration_ms, error, size_bytes
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			event.Timestamp.UnixNano(), timestamp, event.ProcessID, event.TraceID,
			event.Direction, event.Method, event.URL, event.StatusCode, string(headers),
			event.ContentLength, event.DurationMS, event.Error, size)
	case event.Protocol == "websocket" && event.Phase == "handshake":
		result, err = transaction.ExecContext(context.Background(), `INSERT INTO websocket_handshakes (
			timestamp_ns, timestamp, process_id, trace_id, direction, url,
			status_code, headers_json, duration_ms, error, size_bytes
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			event.Timestamp.UnixNano(), timestamp, event.ProcessID, event.TraceID,
			event.Direction, event.URL, event.StatusCode, string(headers),
			event.DurationMS, event.Error, size)
	case event.Protocol == "websocket" && event.Phase == "frame":
		result, err = transaction.ExecContext(context.Background(), `INSERT INTO websocket_frames (
			timestamp_ns, timestamp, process_id, trace_id, direction, url,
			message_type, content_length, error, size_bytes
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			event.Timestamp.UnixNano(), timestamp, event.ProcessID, event.TraceID,
			event.Direction, event.URL, event.MessageType, event.ContentLength,
			event.Error, size)
	default:
		return 0, fmt.Errorf("unsupported traffic event %s/%s", event.Protocol, event.Phase)
	}
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil || len(event.Body) == 0 {
		return size, err
	}
	switch {
	case event.Protocol == "http" && event.Phase == "request":
		_, err = transaction.ExecContext(context.Background(), `INSERT INTO http_request_payloads(request_id, body, body_encoding) VALUES (?, ?, ?)`, id, []byte(event.Body), event.BodyEncoding)
	case event.Protocol == "http" && event.Phase == "response":
		_, err = transaction.ExecContext(context.Background(), `INSERT INTO http_response_payloads(response_id, body, body_encoding) VALUES (?, ?, ?)`, id, []byte(event.Body), event.BodyEncoding)
	case event.Protocol == "websocket" && event.Phase == "frame":
		_, err = transaction.ExecContext(context.Background(), `INSERT INTO websocket_frame_payloads(frame_id, body, body_encoding) VALUES (?, ?, ?)`, id, []byte(event.Body), event.BodyEncoding)
	}
	return size, err
}

func isTrafficBusy(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy") || strings.Contains(message, "sqlite_busy") || strings.Contains(message, "sqlite_locked")
}

func enforceTrafficRetention(transaction *sql.Tx) error {
	var total int64
	if err := transaction.QueryRowContext(context.Background(), `SELECT value FROM traffic_meta WHERE key = 'total_bytes'`).Scan(&total); err != nil {
		return err
	}
	for total > trafficRetentionBytes {
		var table string
		var id, size int64
		err := transaction.QueryRowContext(context.Background(), `SELECT storage_type, id, size_bytes FROM (
			SELECT 'http_requests' AS storage_type, id, timestamp_ns, size_bytes FROM http_requests
			UNION ALL SELECT 'http_responses', id, timestamp_ns, size_bytes FROM http_responses
			UNION ALL SELECT 'websocket_handshakes', id, timestamp_ns, size_bytes FROM websocket_handshakes
			UNION ALL SELECT 'websocket_frames', id, timestamp_ns, size_bytes FROM websocket_frames
		) ORDER BY timestamp_ns, id LIMIT 1`).Scan(&table, &id, &size)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		var count int
		if err := transaction.QueryRowContext(context.Background(), `SELECT
			(SELECT COUNT(*) FROM http_requests) +
			(SELECT COUNT(*) FROM http_responses) +
			(SELECT COUNT(*) FROM websocket_handshakes) +
			(SELECT COUNT(*) FROM websocket_frames)`).Scan(&count); err != nil {
			return err
		}
		if count <= 1 {
			break
		}
		statement := "DELETE FROM " + table + " WHERE id = ?"
		if _, err := transaction.ExecContext(context.Background(), statement, id); err != nil {
			return err
		}
		total -= size
		if _, err := transaction.ExecContext(context.Background(), `UPDATE traffic_meta SET value = ? WHERE key = 'total_bytes'`, total); err != nil {
			return err
		}
	}
	return nil
}

func QueryTraffic(ctx context.Context, database *sql.DB, query TrafficQuery) ([]TrafficEvent, error) {
	if database == nil {
		return nil, errors.New("traffic database is nil")
	}
	conditions := []string{"1 = 1"}
	arguments := make([]any, 0, 8)
	if query.ID > 0 {
		conditions = append(conditions, "id = ?")
		arguments = append(arguments, query.ID)
	}
	if query.Search != "" {
		conditions = append(conditions, `(method LIKE ? OR url LIKE ? OR headers_json LIKE ? OR CAST(body AS TEXT) LIKE ? OR error LIKE ?)`)
		search := "%" + query.Search + "%"
		arguments = append(arguments, search, search, search, search, search)
	}
	for column, value := range map[string]string{"protocol": query.Protocol, "direction": query.Direction, "phase": query.Phase} {
		if value != "" {
			conditions = append(conditions, column+" = ?")
			arguments = append(arguments, value)
		}
	}
	if !query.Since.IsZero() {
		conditions = append(conditions, "timestamp_ns >= ?")
		arguments = append(arguments, query.Since.UnixNano())
	}
	if !query.Until.IsZero() {
		conditions = append(conditions, "timestamp_ns <= ?")
		arguments = append(arguments, query.Until.UnixNano())
	}
	sortOrder := "DESC"
	if strings.EqualFold(query.Sort, "asc") {
		sortOrder = "ASC"
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 50
	}
	arguments = append(arguments, limit)
	statement := `SELECT id, timestamp, process_id, trace_id, protocol, direction, phase, method, url,
		status_code, headers_json, body, body_encoding, content_length, duration_ms,
		message_type, error
		FROM traffic_events WHERE ` + strings.Join(conditions, " AND ") +
		" ORDER BY timestamp_ns " + sortOrder + ", id " + sortOrder + " LIMIT ?"
	rows, err := database.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []TrafficEvent
	for rows.Next() {
		var timestamp string
		var headersJSON string
		var body []byte
		var event TrafficEvent
		if err := rows.Scan(&event.ID, &timestamp, &event.ProcessID, &event.TraceID, &event.Protocol, &event.Direction, &event.Phase, &event.Method, &event.URL, &event.StatusCode, &headersJSON, &body, &event.BodyEncoding, &event.ContentLength, &event.DurationMS, &event.MessageType, &event.Error); err != nil {
			return nil, err
		}
		event.Timestamp, _ = time.Parse(time.RFC3339Nano, timestamp)
		_ = json.Unmarshal([]byte(headersJSON), &event.Headers)
		if query.IncludeBody {
			bodyLimit := query.BodyLimit
			if bodyLimit <= 0 || bodyLimit > len(body) {
				bodyLimit = len(body)
			}
			event.Body = string(body[:bodyLimit])
			if bodyLimit < len(body) {
				event.Body += fmt.Sprintf("\n[truncated %d bytes]", len(body)-bodyLimit)
			}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

const EphemeralStateHeader = "X-Crux-Ephemeral-State"

func WrapHTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = defaultHTTPBase
	}
	if _, ok := base.(*networkRoundTripper); ok {
		return base
	}
	return &networkRoundTripper{base: base}
}

func CloneDefaultHTTPTransport() *http.Transport {
	return defaultHTTPBase.Clone()
}

type networkRoundTripper struct {
	base http.RoundTripper
}

func (t *networkRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("HTTP request is nil")
	}
	trace := networkTraceState.Load()
	if trace == nil {
		return t.base.RoundTrip(request)
	}
	traceID := nextNetworkTraceID()
	requestEvent := TrafficEvent{Timestamp: time.Now().UTC(), TraceID: traceID, Protocol: "http", Direction: "outbound", Phase: "request", Method: request.Method, URL: sanitizeURL(request.URL), Headers: formatHeaders(request.Header), ContentLength: request.ContentLength}
	if request.Header.Get(EphemeralStateHeader) != "" {
		trace.record(requestEvent)
	} else {
		body, copied, copyErr := copyRequestBody(request)
		if copied {
			requestEvent.Body, requestEvent.BodyEncoding = encodeNetworkBody(body, request.Header.Get("Content-Type"))
			if copyErr != nil {
				requestEvent.Error = copyErr.Error()
			}
			trace.record(requestEvent)
		} else {
			request.Body = &networkTraceBody{ReadCloser: request.Body, trace: trace, event: requestEvent}
		}
	}

	started := time.Now()
	response, err := t.base.RoundTrip(request)
	duration := time.Since(started).Milliseconds()
	if err != nil {
		trace.record(TrafficEvent{TraceID: traceID, Protocol: "http", Direction: "inbound", Phase: "response", Method: request.Method, URL: sanitizeURL(request.URL), DurationMS: duration, Error: err.Error()})
		return response, err
	}
	event := TrafficEvent{TraceID: traceID, Protocol: "http", Direction: "inbound", Phase: "response", Method: request.Method, URL: sanitizeURL(request.URL), StatusCode: response.StatusCode, Headers: formatHeaders(response.Header), ContentLength: response.ContentLength, DurationMS: duration}
	if response.Body == nil || response.Body == http.NoBody {
		trace.record(event)
	} else {
		response.Body = &networkTraceBody{ReadCloser: response.Body, trace: trace, event: event}
	}
	return response, nil
}

type trafficCapture struct {
	buffer    bytes.Buffer
	total     int64
	truncated bool
}

func (c *trafficCapture) Write(data []byte) (int, error) {
	c.total += int64(len(data))
	remaining := trafficMaxPayloadBytes - c.buffer.Len()
	if remaining > 0 {
		stored := len(data)
		if stored > remaining {
			stored = remaining
		}
		_, _ = c.buffer.Write(data[:stored])
	}
	if c.total > int64(c.buffer.Len()) {
		c.truncated = true
	}
	return len(data), nil
}

func (c *trafficCapture) Bytes() []byte {
	return c.buffer.Bytes()
}

func (c *trafficCapture) Encode(contentType string) (string, string) {
	body, encoding := encodeNetworkBody(c.Bytes(), contentType)
	if c.truncated && !strings.HasSuffix(encoding, "-truncated") {
		encoding += "-truncated"
	}
	return body, encoding
}

type networkTraceBody struct {
	io.ReadCloser
	trace *networkTrace
	event TrafficEvent
	body  trafficCapture
	once  sync.Once
}

func (b *networkTraceBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if n > 0 {
		_, _ = b.body.Write(data[:n])
	}
	if err != nil {
		b.record(err)
	}
	return n, err
}

func (b *networkTraceBody) Close() error {
	err := b.ReadCloser.Close()
	b.record(err)
	return err
}

func (b *networkTraceBody) record(err error) {
	b.once.Do(func() {
		b.event.Body, b.event.BodyEncoding = b.body.Encode(http.Header(b.event.Headers).Get("Content-Type"))
		b.event.ContentLength = b.body.total
		if err != nil && !errors.Is(err, io.EOF) {
			b.event.Error = err.Error()
		}
		b.trace.record(b.event)
	})
}

func TraceHTTPHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		trace := networkTraceState.Load()
		if trace == nil {
			next.ServeHTTP(writer, request)
			return
		}
		traceID := nextNetworkTraceID()
		started := time.Now()
		wrapped := &networkResponseWriter{ResponseWriter: writer, statusCode: http.StatusOK}
		requestEvent := TrafficEvent{Timestamp: started.UTC(), TraceID: traceID, Protocol: "http", Direction: "inbound", Phase: "request", Method: request.Method, URL: sanitizeURL(request.URL), Headers: formatHeaders(request.Header), ContentLength: request.ContentLength}
		if request.Body == nil || request.Body == http.NoBody || request.Header.Get(EphemeralStateHeader) != "" {
			trace.record(requestEvent)
		} else {
			request.Body = &networkTraceBody{ReadCloser: request.Body, trace: trace, event: requestEvent}
		}
		next.ServeHTTP(wrapped, request)
		if request.Body != nil {
			_ = request.Body.Close()
		}
		responseBody, responseEncoding := wrapped.body.Encode(wrapped.Header().Get("Content-Type"))
		trace.record(TrafficEvent{TraceID: traceID, Protocol: "http", Direction: "outbound", Phase: "response", Method: request.Method, URL: sanitizeURL(request.URL), StatusCode: wrapped.statusCode, Headers: formatHeaders(wrapped.Header()), Body: responseBody, BodyEncoding: responseEncoding, ContentLength: wrapped.body.total, DurationMS: time.Since(started).Milliseconds()})
	})
}

type networkResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	body        trafficCapture
}

func (w *networkResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *networkResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = w.body.Write(data)
	return w.ResponseWriter.Write(data)
}

func (w *networkResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{w}, reader)
}

func (w *networkResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *networkResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.statusCode = http.StatusSwitchingProtocols
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *networkResponseWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

func (w *networkResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func TraceWebSocketFrame(traceID, direction, endpoint string, messageType int, data []byte, err error) {
	trace := networkTraceState.Load()
	if trace == nil {
		return
	}
	body, encoding := encodeNetworkBody(data, "application/json")
	event := TrafficEvent{TraceID: traceID, Protocol: "websocket", Direction: direction, Phase: "frame", URL: sanitizeURLString(endpoint), MessageType: messageType, Body: body, BodyEncoding: encoding, ContentLength: int64(len(data))}
	if err != nil {
		event.Error = err.Error()
	}
	trace.record(event)
}

func TraceWebSocketHandshake(traceID, direction, endpoint string, headers http.Header, statusCode int, duration time.Duration, err error) {
	trace := networkTraceState.Load()
	if trace == nil {
		return
	}
	event := TrafficEvent{TraceID: traceID, Protocol: "websocket", Direction: direction, Phase: "handshake", URL: sanitizeURLString(endpoint), Headers: formatHeaders(headers), StatusCode: statusCode, DurationMS: duration.Milliseconds()}
	if err != nil {
		event.Error = err.Error()
	}
	trace.record(event)
}

func copyRequestBody(request *http.Request) ([]byte, bool, error) {
	if request == nil || request.Body == nil || request.Body == http.NoBody {
		return nil, true, nil
	}
	if request.GetBody == nil {
		return nil, false, nil
	}
	body, err := request.GetBody()
	if err != nil {
		return nil, true, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, trafficMaxPayloadBytes+1))
	return data, true, err
}

func encodeNetworkBody(data []byte, contentType string) (string, string) {
	if len(data) == 0 {
		return "", ""
	}
	truncated := len(data) > trafficMaxPayloadBytes
	data = redact.Bytes(data)
	if len(data) > trafficMaxPayloadBytes {
		data = data[:trafficMaxPayloadBytes]
	}
	encoding := "utf-8"
	if !utf8.Valid(data) {
		encoding = "base64"
		data = []byte(base64.StdEncoding.EncodeToString(data))
	} else if !truncated {
		data = redactNetworkBody(data, contentType)
	}
	if truncated {
		encoding += "-truncated"
	}
	return string(data), encoding
}

func redactNetworkBody(data []byte, contentType string) []byte {
	var value any
	if json.Unmarshal(data, &value) == nil && redactNetworkJSON(value) {
		if redacted, err := json.Marshal(value); err == nil {
			return redacted
		}
	}
	if strings.HasPrefix(strings.ToLower(contentType), "application/x-www-form-urlencoded") {
		form, err := url.ParseQuery(string(data))
		if err == nil {
			changed := false
			for key := range form {
				if sensitiveBodyName(key) {
					form.Set(key, "[REDACTED]")
					changed = true
				}
			}
			if changed {
				return []byte(form.Encode())
			}
		}
	}
	return data
}

func redactNetworkJSON(value any) bool {
	changed := false
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if sensitiveBodyName(key) {
				typed[key] = "[REDACTED]"
				changed = true
				continue
			}
			if redactNetworkJSON(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range typed {
			if redactNetworkJSON(child) {
				changed = true
			}
		}
	}
	return changed
}

func sensitiveBodyName(name string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(name))
	switch normalized {
	case "authorization", "apikey", "token", "accesstoken", "refreshtoken", "idtoken", "clientsecret", "password", "credential", "credentials", "cookie", "setcookie":
		return true
	default:
		return false
	}
}

func sanitizeURL(raw *url.URL) string {
	if raw == nil {
		return ""
	}
	clone := *raw
	query := clone.Query()
	for key := range query {
		if sensitiveName(key) {
			query.Set(key, "[REDACTED]")
		}
	}
	clone.RawQuery = query.Encode()
	return redact.String(clone.String())
}

func sanitizeURLString(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return redact.String(raw)
	}
	return sanitizeURL(parsed)
}

func sensitiveName(name string) bool {
	name = strings.ToLower(name)
	return strings.Contains(name, "authorization") || strings.Contains(name, "api-key") || strings.Contains(name, "api_key") || strings.Contains(name, "apikey") || strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "password") || strings.Contains(name, "credential") || strings.Contains(name, "cookie")
}

func NewWebSocketTraceID() string {
	return nextNetworkTraceID()
}

func nextNetworkTraceID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), networkTraceID.Add(1))
}
