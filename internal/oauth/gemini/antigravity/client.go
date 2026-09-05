package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
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

const defaultMaxEventBytes = int64(1 << 20)

type client struct {
	httpClient    *http.Client
	baseURL       string
	token         TokenSource
	project       ProjectLoader
	userAgent     string
	headers       map[string]string
	maxEventBytes int64
	retry         manifest.RetryPolicy
	errors        []manifest.ErrorMapping
}

type retryBudget struct {
	policy   manifest.RetryPolicy
	attempts int
}

func defaultRetryPolicy() manifest.RetryPolicy {
	return manifest.RetryPolicy{
		MaxAttempts:       7,
		InitialDelayMS:    2000,
		MaxDelayMS:        30000,
		Factor:            2,
		Statuses:          []int{http.StatusTooManyRequests, http.StatusServiceUnavailable},
		TransportErrors:   true,
		UnexpectedEOF:     true,
		Authentication:    "refresh-once",
		ReplayRequirement: "before-first-event",
	}
}

func (c *client) effectiveRetryPolicy() manifest.RetryPolicy {
	if c.retry.MaxAttempts > 0 {
		return c.retry
	}
	return defaultRetryPolicy()
}

func newRetryBudget(policy manifest.RetryPolicy) *retryBudget {
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	return &retryBudget{policy: policy}
}

func (budget *retryBudget) start() bool {
	if budget.attempts >= budget.policy.MaxAttempts {
		return false
	}
	budget.attempts++
	return true
}

func (budget *retryBudget) retry(ctx context.Context, retryable, emitted bool, retryAfter string) (bool, error) {
	if !retryable || budget.attempts >= budget.policy.MaxAttempts || budget.policy.ReplayRequirement == "never" || budget.policy.ReplayRequirement == "before-first-event" && emitted {
		return false, nil
	}
	if !budget.policy.RetryAfter {
		retryAfter = ""
	}
	if err := providertransport.WaitForRetry(ctx, providertransport.RetryDelay(budget.policy, budget.attempts, retryAfter)); err != nil {
		return false, err
	}
	return true, nil
}

func retryBodyError(policy manifest.RetryPolicy, err error) bool {
	if policy.UnexpectedEOF && errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if !policy.TransportErrors {
		return false
	}
	if errors.Is(err, providertransport.ErrStreamIdleTimeout) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
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
func (c *client) do(ctx context.Context, method string, stream bool, env *wireEnvelope, budget *retryBudget) (*http.Response, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}

	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !budget.start() {
			return nil, fmt.Errorf("antigravity: retry budget exhausted")
		}

		req, err := c.newRequest(ctx, method, stream, body)
		if err != nil {
			return nil, err
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if providertransport.IsOwnerValidationError(err) {
				return nil, err
			}
			transportErr := fantasy.WrapTransportError(err)
			retry, waitErr := budget.retry(ctx, budget.policy.TransportErrors, false, "")
			if waitErr != nil {
				return nil, waitErr
			}
			if retry {
				continue
			}
			return nil, transportErr
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		retryAfter := resp.Header.Get("Retry-After")
		perr := parseErrorResponse(resp)
		providertransport.MapError(c.errors, perr)

		slog.Debug("Antigravity request failed",
			"status", resp.StatusCode,
			"model", env.Model,
			"project", env.Project,
			"user_agent", c.userAgent,
			"response", string(perr.ResponseBody),
			"request", string(body),
		)

		standardRetryable := providertransport.RetryOperationError(budget.policy, nil, perr, false) && !isHardCapacityExhaustion(resp.StatusCode, perr)
		retryable := standardRetryable || providertransport.ErrorMappingRetryable(c.errors, perr)
		retry, waitErr := budget.retry(ctx, retryable, false, retryAfter)
		if waitErr != nil {
			return nil, waitErr
		}
		if retry {
			continue
		}
		return nil, perr
	}
}

// isRetryableOverload reports whether the failure is transient capacity
// pressure. Hard model-capacity exhaustion is not retryable.
func isHardCapacityExhaustion(status int, perr *fantasy.ProviderError) bool {
	return status == http.StatusTooManyRequests && strings.Contains(string(perr.ResponseBody), "MODEL_CAPACITY_EXHAUSTED")
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
		Title:           titleForStatus(resp.StatusCode),
		Message:         message,
		StatusCode:      resp.StatusCode,
		ResponseHeaders: responseHeaders(resp.Header),
		ResponseBody:    data,
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

func responseHeaders(headers http.Header) map[string]string {
	values := make(map[string]string, len(headers))
	for name := range headers {
		values[name] = headers.Get(name)
	}
	return values
}

func wireProviderError(value *wireError, body []byte) *fantasy.ProviderError {
	perr := &fantasy.ProviderError{
		Title:        titleForStatus(value.Code),
		Message:      value.Message,
		StatusCode:   value.Code,
		ResponseBody: body,
	}
	parseContextTooLargeError(value.Message, perr)
	return perr
}

// generateContent performs a non-streaming call.
func (c *client) generateContent(ctx context.Context, model string, req *wireRequest) (*wireResponse, error) {
	env := c.envelope(ctx, model, req)
	budget := newRetryBudget(c.effectiveRetryPolicy())
	for {
		resp, err := c.do(ctx, "generateContent", false, env, budget)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			retry, waitErr := budget.retry(ctx, retryBodyError(budget.policy, readErr), false, "")
			if waitErr != nil {
				return nil, waitErr
			}
			if retry {
				continue
			}
			return nil, fantasy.WrapTransportError(readErr)
		}
		out := new(wireResponse)
		if err := json.Unmarshal(unwrapPayload(data), out); err != nil {
			return nil, fmt.Errorf("antigravity: decode response: %w", err)
		}
		if out.Error != nil {
			perr := wireProviderError(out.Error, unwrapPayload(data))
			providertransport.MapError(c.errors, perr)
			retry, waitErr := budget.retry(ctx, providertransport.RetryOperationError(budget.policy, c.errors, perr, false), false, providertransport.RetryAfterHeader(perr))
			if waitErr != nil {
				return nil, waitErr
			}
			if retry {
				continue
			}
			return nil, perr
		}
		return out, nil
	}
}

// streamGenerateContent performs a streaming call, yielding decoded chunks.
func (c *client) streamGenerateContent(ctx context.Context, model string, req *wireRequest) (iter.Seq2[*wireResponse, error], error) {
	env := c.envelope(ctx, model, req)
	budget := newRetryBudget(c.effectiveRetryPolicy())
	resp, err := c.do(ctx, "streamGenerateContent", true, env, budget) //nolint:bodyclose
	if err != nil {
		return nil, err
	}
	return func(yield func(*wireResponse, error) bool) {
		current := resp
		emitted := false
		for {
			sawPayload := false
			replay := false
			for payload, readErr := range sseEvents(current.Body, c.maxEventBytes) {
				if readErr != nil {
					_ = current.Body.Close()
					retry, waitErr := budget.retry(ctx, retryBodyError(budget.policy, readErr), emitted, "")
					if waitErr != nil {
						yield(nil, waitErr)
						return
					}
					if retry {
						next, openErr := c.do(ctx, "streamGenerateContent", true, env, budget) //nolint:bodyclose
						if openErr != nil {
							yield(nil, openErr)
							return
						}
						current = next
						replay = true
						break
					}
					yield(nil, fantasy.WrapTransportError(readErr))
					return
				}
				sawPayload = true
				chunk := new(wireResponse)
				if err := json.Unmarshal(unwrapPayload(payload), chunk); err != nil {
					_ = current.Body.Close()
					yield(nil, fmt.Errorf("antigravity: decode chunk: %w", err))
					return
				}
				if chunk.Error != nil {
					_ = current.Body.Close()
					perr := wireProviderError(chunk.Error, unwrapPayload(payload))
					providertransport.MapError(c.errors, perr)
					retry, waitErr := budget.retry(ctx, providertransport.RetryOperationError(budget.policy, c.errors, perr, emitted), emitted, providertransport.RetryAfterHeader(perr))
					if waitErr != nil {
						yield(nil, waitErr)
						return
					}
					if retry {
						next, openErr := c.do(ctx, "streamGenerateContent", true, env, budget) //nolint:bodyclose
						if openErr != nil {
							yield(nil, openErr)
							return
						}
						current = next
						replay = true
						break
					}
					yield(nil, perr)
					return
				}
				if !yield(chunk, nil) {
					_ = current.Body.Close()
					return
				}
				emitted = true
			}
			if replay {
				continue
			}
			_ = current.Body.Close()
			if sawPayload || emitted || !budget.policy.UnexpectedEOF {
				return
			}
			retry, waitErr := budget.retry(ctx, true, false, "")
			if waitErr != nil {
				yield(nil, waitErr)
				return
			}
			if !retry {
				yield(nil, fantasy.WrapTransportError(io.ErrUnexpectedEOF))
				return
			}
			next, openErr := c.do(ctx, "streamGenerateContent", true, env, budget)
			if openErr != nil {
				yield(nil, openErr)
				return
			}
			current = next
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
func sseEvents(body io.Reader, maxEventBytes int64) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		if maxEventBytes <= 0 {
			maxEventBytes = defaultMaxEventBytes
		}
		marker := []byte("data:")
		var buf []byte
		chunk := make([]byte, 4096)

		drain := func(terminalErr error) bool {
			for {
				i := bytes.Index(buf, marker)
				if i < 0 {
					if terminalErr != nil {
						buf = nil
						return true
					}
					keep := min(len(buf), len(marker)-1)
					for keep > 0 && !bytes.HasPrefix(marker, buf[len(buf)-keep:]) {
						keep--
					}
					buf = append(buf[:0], buf[len(buf)-keep:]...)
					return true
				}
				raw := buf[i+len(marker):]
				rest := bytes.TrimLeft(raw, " \t")
				if int64(len(raw)-len(rest)) > maxEventBytes {
					yield(nil, fmt.Errorf("SSE event exceeds %d bytes", maxEventBytes))
					return false
				}

				if after, ok := bytes.CutPrefix(rest, []byte("[DONE]")); ok {
					buf = after
					continue
				}

				end, complete := jsonValueEnd(rest)
				if int64(end) > maxEventBytes {
					yield(nil, fmt.Errorf("SSE event exceeds %d bytes", maxEventBytes))
					return false
				}
				if !complete {
					if terminalErr != nil {
						if end > 0 {
							if errors.Is(terminalErr, io.EOF) {
								terminalErr = io.ErrUnexpectedEOF
							}
							yield(nil, terminalErr)
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
				if !drain(nil) {
					return
				}
			}
			if err != nil {
				if !drain(err) {
					return
				}
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
