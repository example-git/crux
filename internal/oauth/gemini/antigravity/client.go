package antigravity

// Native HTTP client for the Antigravity Cloud Code endpoint. It builds the
// request envelope itself, streams SSE responses, and applies the retry
// policy observed to keep the endpoint stable:
//
//   - network errors: up to 3 retries with exponential backoff;
//   - 429 RESOURCE_EXHAUSTED and 503: retried with capped backoff;
//   - 429 MODEL_CAPACITY_EXHAUSTED: fails immediately (hard quota);
//   - 401/403: token re-read once from the token source (picking up any
//     refresh performed by the credential store), then fails.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/google/uuid"
)

// TokenSource supplies the current OAuth access token per request so
// refreshed credentials are picked up without rebuilding the provider.
type TokenSource = func() string

// ProjectLoader resolves the Cloud AI Companion project bound to the current
// credential; required in the request envelope.
type ProjectLoader = func(ctx context.Context, token string) string

// Stable per-process identifiers for the Antigravity requestId format:
// agent/<agentId>/<startMs>/<sessionId>/<seqNum>.
var (
	agentID      = uuid.NewString()
	agentStartMS = time.Now().UnixMilli()
	sessionUUID  = uuid.NewString()
	requestSeq   atomic.Uint64
)

func nextNumericSessionID() string {
	return fmt.Sprintf("%d", -(rand.Int64N(9_000_000_000_000_000) + 1_000_000_000_000_000))
}

const (
	maxNetworkRetries  = 3
	maxOverloadRetries = 6
	maxBackoff         = 30 * time.Second
)

type client struct {
	httpClient *http.Client
	baseURL    string
	token      TokenSource
	project    ProjectLoader
	userAgent  string
	headers    map[string]string
}

func (c *client) envelope(ctx context.Context, model string, req *wireRequest) *wireEnvelope {
	token := ""
	if c.token != nil {
		token = c.token()
	}
	project := ""
	if c.project != nil {
		project = c.project(ctx, token)
	}
	req.SessionID = nextNumericSessionID()
	return &wireEnvelope{
		Project:     project,
		RequestID:   fmt.Sprintf("agent/%s/%d/%s/%d", agentID, agentStartMS, sessionUUID, requestSeq.Add(1)),
		Request:     req,
		Model:       model,
		UserAgent:   "antigravity",
		RequestType: "agent",
	}
}

func (c *client) newRequest(ctx context.Context, method string, stream bool, body []byte) (*http.Request, error) {
	url := strings.TrimSuffix(c.baseURL, "/") + "/v1internal:" + method
	if stream {
		url += "?alt=sse"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	// The Antigravity endpoint licenses by client identity: the User-Agent
	// must always be the Antigravity CLI UA, never a generic client UA from
	// configured extra headers.
	req.Header.Set("User-Agent", c.userAgent)
	if c.token != nil {
		if token := c.token(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	return req, nil
}

// do performs the request with the retry policy and returns a response whose
// status is 2xx. Non-retryable failures are returned as *fantasy.ProviderError.
func (c *client) do(ctx context.Context, method string, stream bool, env *wireEnvelope) (*http.Response, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}

	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	var networkAttempt, overloadAttempt int
	authRetried := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		req, err := c.newRequest(ctx, method, stream, body)
		if err != nil {
			return nil, err
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			networkAttempt++
			if networkAttempt <= maxNetworkRetries && ctx.Err() == nil {
				if !sleepCtx(ctx, backoff(networkAttempt)) {
					return nil, ctx.Err()
				}
				continue
			}
			return nil, fantasy.WrapTransportError(err)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		perr := parseErrorResponse(resp)

		slog.Debug("Antigravity request failed",
			"status", resp.StatusCode,
			"model", env.Model,
			"project", env.Project,
			"user_agent", c.userAgent,
			"response", string(perr.ResponseBody),
			"request", string(body),
		)

		switch {
		case (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && !authRetried:
			// The token source may return a refreshed credential; retry once.
			authRetried = true
			continue
		case isRetryableOverload(resp.StatusCode, perr):
			overloadAttempt++
			if overloadAttempt <= maxOverloadRetries && ctx.Err() == nil {
				if !sleepCtx(ctx, backoff(overloadAttempt)) {
					return nil, ctx.Err()
				}
				continue
			}
			return nil, perr
		default:
			return nil, perr
		}
	}
}

func backoff(attempt int) time.Duration {
	return min(time.Second<<attempt, maxBackoff)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isRetryableOverload reports whether the failure is transient capacity
// pressure. Hard model-capacity exhaustion is not retryable.
func isRetryableOverload(status int, perr *fantasy.ProviderError) bool {
	if status == http.StatusServiceUnavailable {
		return true
	}
	if status != http.StatusTooManyRequests {
		return false
	}
	return !strings.Contains(string(perr.ResponseBody), "MODEL_CAPACITY_EXHAUSTED")
}

// parseErrorResponse drains a non-2xx response into a ProviderError.
func parseErrorResponse(resp *http.Response) *fantasy.ProviderError {
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	message := fmt.Sprintf("antigravity: HTTP %d", resp.StatusCode)
	// The endpoint returns either {"error": {...}} or [{"error": {...}}].
	payload := bytes.TrimSpace(data)
	if len(payload) > 0 && payload[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(payload, &arr); err == nil && len(arr) > 0 {
			payload = arr[0]
		}
	}
	var parsed struct {
		Error *wireError `json:"error"`
	}
	if err := json.Unmarshal(payload, &parsed); err == nil && parsed.Error != nil && parsed.Error.Message != "" {
		message = parsed.Error.Message
	}

	perr := &fantasy.ProviderError{
		Title:        titleForStatus(resp.StatusCode),
		Message:      message,
		StatusCode:   resp.StatusCode,
		ResponseBody: data,
	}
	parseContextTooLargeError(message, perr)
	return perr
}

func titleForStatus(status int) string {
	if t := fantasy.ErrorTitleForStatusCode(status); t != "" {
		return t
	}
	return "provider request failed"
}

// generateContent performs a non-streaming call.
func (c *client) generateContent(ctx context.Context, model string, req *wireRequest) (*wireResponse, error) {
	resp, err := c.do(ctx, "generateContent", false, c.envelope(ctx, model, req))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fantasy.WrapTransportError(err)
	}
	out := new(wireResponse)
	if err := json.Unmarshal(unwrapPayload(data), out); err != nil {
		return nil, fmt.Errorf("antigravity: decode response: %w", err)
	}
	if out.Error != nil {
		return nil, &fantasy.ProviderError{
			Title:      titleForStatus(out.Error.Code),
			Message:    out.Error.Message,
			StatusCode: out.Error.Code,
		}
	}
	return out, nil
}

// streamGenerateContent performs a streaming call, yielding decoded chunks.
func (c *client) streamGenerateContent(ctx context.Context, model string, req *wireRequest) (iter.Seq2[*wireResponse, error], error) {
	// The returned iterator owns and closes the streaming response body when
	// consumed; closing it here would invalidate the stream before iteration.
	resp, err := c.do(ctx, "streamGenerateContent", true, c.envelope(ctx, model, req)) //nolint:bodyclose
	if err != nil {
		return nil, err
	}
	return func(yield func(*wireResponse, error) bool) {
		defer resp.Body.Close()
		for payload, err := range sseEvents(resp.Body) {
			if err != nil {
				yield(nil, fantasy.WrapTransportError(err))
				return
			}
			chunk := new(wireResponse)
			if err := json.Unmarshal(unwrapPayload(payload), chunk); err != nil {
				yield(nil, fmt.Errorf("antigravity: decode chunk: %w", err))
				return
			}
			if chunk.Error != nil {
				yield(nil, &fantasy.ProviderError{
					Title:      titleForStatus(chunk.Error.Code),
					Message:    chunk.Error.Message,
					StatusCode: chunk.Error.Code,
				})
				return
			}
			if !yield(chunk, nil) {
				return
			}
		}
	}, nil
}

// unwrapPayload lifts {"response": {...}} to {...}. Payloads already in the
// expected shape are returned unchanged.
func unwrapPayload(data []byte) []byte {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return data
	}
	inner, ok := probe["response"]
	if !ok || len(inner) == 0 {
		return data
	}
	return inner
}

// sseEvents yields the JSON payload of each SSE event in the body.
//
// Events are located by scanning for "data:" and consuming exactly one
// balanced JSON value rather than splitting on newlines: the endpoint
// sometimes emits consecutive events with no separator at all
// ("...}data: {..."), which a line-based reader would treat as one
// malformed payload.
func sseEvents(body io.Reader) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		var buf []byte
		chunk := make([]byte, 4096)

		// drain consumes every complete event in buf. When atEOF is false it
		// leaves a trailing partial event in place for the next read.
		drain := func(atEOF bool) bool {
			for {
				i := bytes.Index(buf, []byte("data:"))
				if i < 0 {
					if atEOF {
						buf = nil
					}
					return true
				}
				rest := bytes.TrimLeft(buf[i+len("data:"):], " \t")

				// Terminator events carry no JSON.
				if after, ok := bytes.CutPrefix(rest, []byte("[DONE]")); ok {
					buf = after
					continue
				}

				end, complete := jsonValueEnd(rest)
				if !complete {
					if atEOF {
						// Truncated tail: surface what we have so the caller
						// reports a parse error rather than hanging.
						if end > 0 && !yield(rest[:end], nil) {
							return false
						}
						buf = nil
						return true
					}
					buf = buf[i:]
					return true
				}

				if !yield(rest[:end], nil) {
					return false
				}
				buf = rest[end:]
			}
		}

		for {
			n, err := body.Read(chunk)
			if n > 0 {
				buf = append(buf, chunk[:n]...)
				if !drain(false) {
					return
				}
			}
			if err != nil {
				_ = drain(true)
				if err != io.EOF {
					yield(nil, err)
				}
				return
			}
		}
	}
}

// jsonValueEnd returns the offset just past the first balanced JSON object or
// array in data, and whether such a value was found in full. Brace counting
// skips over string contents and escapes.
func jsonValueEnd(data []byte) (end int, complete bool) {
	depth := 0
	inString := false
	escaped := false

	for i := range data {
		c := data[i]

		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}

		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return len(data), false
}
