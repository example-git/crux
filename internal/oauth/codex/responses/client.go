package responses

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// TokenSource supplies the current OAuth access token per request so
// refreshed credentials are picked up without rebuilding the provider.
type TokenSource = func() string

// AccountIDSource supplies the ChatGPT account id paired with the current
// OAuth credentials. Codex stores this separately from the access token.
type AccountIDSource = func() string

const (
	openaiBeta                       = "responses_websockets=2026-02-06"
	remoteCompactionV2BetaFeature    = "remote_compaction_v2"
	defaultWriteTimeout              = 30 * time.Second
	defaultReadIdleTimeout           = 5 * time.Minute
	defaultMaxEventBytes             = int64(1 << 20)
	readPumpBufferSize               = 1600
	stateLockPollInterval            = 10 * time.Millisecond
	abnormalClosureInitialRetryDelay = 100 * time.Millisecond
	abnormalClosureMaxRetryDelay     = 5 * time.Second
)

// installationID is a stable per-process Codex installation identifier.
var installationID = uuid.NewString()

type client struct {
	url            string
	token          TokenSource
	accountID      AccountIDSource
	userAgent      string
	originator     string
	version        string
	headers        map[string]string
	sessionStore   *SessionStore
	ownerValidator func() error
	connectTimeout time.Duration
	requestTimeout time.Duration
	writeTimeout   time.Duration
	readTimeout    time.Duration
	maxEventBytes  int64
	retry          manifest.RetryPolicy
	errors         []manifest.ErrorMapping
	compaction     executionProfile
	requestBudget  requestBudget
}

type executionProfile struct {
	connectTimeout time.Duration
	requestTimeout time.Duration
	readTimeout    time.Duration
	maxEventBytes  int64
	retry          manifest.RetryPolicy
	errors         []manifest.ErrorMapping
}

func (p executionProfile) clone() executionProfile {
	p.retry = cloneRetryPolicy(p.retry)
	p.errors = cloneErrorMappings(p.errors)
	return p
}

func cloneRetryPolicy(policy manifest.RetryPolicy) manifest.RetryPolicy {
	policy.Statuses = append([]int(nil), policy.Statuses...)
	policy.Codes = append([]string(nil), policy.Codes...)
	return policy
}

func cloneErrorMappings(mappings []manifest.ErrorMapping) []manifest.ErrorMapping {
	cloned := make([]manifest.ErrorMapping, len(mappings))
	for i := range mappings {
		cloned[i] = mappings[i]
		cloned[i].Statuses = append([]int(nil), mappings[i].Statuses...)
		cloned[i].Codes = append([]string(nil), mappings[i].Codes...)
	}
	return cloned
}

func (c *client) effectiveRequestBudget() requestBudget {
	if c.requestBudget.requestBytes > 0 {
		return c.requestBudget.clone()
	}
	budget, _ := compileRequestBudget(defaultImagePolicyDeclaration())
	return budget
}

func (c *client) inferenceProfile() executionProfile {
	return executionProfile{
		connectTimeout: c.connectTimeout,
		requestTimeout: c.requestTimeout,
		readTimeout:    c.readTimeout,
		maxEventBytes:  c.maxEventBytes,
		retry:          cloneRetryPolicy(c.effectiveRetryPolicy()),
		errors:         cloneErrorMappings(c.errors),
	}
}

func (p executionProfile) effectiveConnectTimeout() time.Duration {
	if p.connectTimeout > 0 {
		return p.connectTimeout
	}
	return 30 * time.Second
}

func (p executionProfile) effectiveReadTimeout() time.Duration {
	if p.readTimeout > 0 {
		return p.readTimeout
	}
	return defaultReadIdleTimeout
}

func (p executionProfile) effectiveMaxEventBytes() int64 {
	if p.maxEventBytes > 0 {
		return p.maxEventBytes
	}
	return defaultMaxEventBytes
}

type websocketReadResult struct {
	data []byte
	err  error
}

// dial opens the WebSocket connection with the Codex identity headers.
func (c *client) dial(ctx context.Context, token, accountID, compatibilityID, traceID string) (*websocket.Conn, error) {
	return c.dialWithProfile(ctx, token, accountID, compatibilityID, traceID, c.inferenceProfile())
}

func (c *client) dialWithProfile(ctx context.Context, token, accountID, compatibilityID, traceID string, profile executionProfile) (*websocket.Conn, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("User-Agent", c.userAgent)
	header.Set("Origin", "https://chatgpt.com")
	header.Set("openai-beta", openaiBeta)
	header.Set("session_id", compatibilityID)
	header.Set("thread-id", compatibilityID)
	header.Set("originator", c.originator)
	header.Set("version", c.version)
	header.Set("x-codex-installation-id", installationID)
	header.Set("x-codex-window-id", compatibilityID+":0")
	header.Set("x-client-request-id", compatibilityID)
	if cwd, err := os.Getwd(); err == nil {
		meta, _ := json.Marshal(map[string]any{
			"installation_id": installationID,
			"session_id":      compatibilityID,
			"thread_id":       compatibilityID,
			"turn_id":         "",
			"window_id":       compatibilityID + ":0",
			"workspaces":      map[string]any{cwd: map[string]any{}},
			"sandbox":         "none",
		})
		header.Set("x-codex-turn-metadata", string(meta))
	}
	if accountID != "" {
		header.Set("chatgpt-account-id", accountID)
	}
	for k, v := range c.headers {
		header.Set(k, v)
	}
	header.Set("x-codex-beta-features", appendHeaderValue(header.Get("x-codex-beta-features"), remoteCompactionV2BetaFeature))

	dialer := websocket.Dialer{HandshakeTimeout: profile.effectiveConnectTimeout()}
	if c.ownerValidator != nil {
		if err := c.ownerValidator(); err != nil {
			return nil, fmt.Errorf("Codex provider owner changed before WebSocket dial: %w", err)
		}
	}
	cruxlog.TraceWebSocketHandshake(traceID, "outbound", c.url, header, 0, 0, nil)
	started := time.Now()
	conn, resp, err := dialer.DialContext(ctx, c.url, header)
	statusCode := 0
	responseHeaders := http.Header(nil)
	var responseBody []byte
	if resp != nil {
		statusCode = resp.StatusCode
		responseHeaders = resp.Header.Clone()
		if resp.Body != nil {
			responseBody, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
		}
	}
	cruxlog.TraceWebSocketHandshake(traceID, "inbound", c.url, responseHeaders, statusCode, time.Since(started), err)
	if err != nil {
		if resp != nil {
			providerErr := &fantasy.ProviderError{
				Title:           "connection failed",
				Message:         fmt.Sprintf("codex: WebSocket handshake failed: %v", err),
				StatusCode:      resp.StatusCode,
				ResponseHeaders: responseHeaderValues(resp.Header),
				ResponseBody:    responseBody,
			}
			providertransport.MapError(profile.errors, providerErr)
			return nil, providerErr
		}
		return nil, fantasy.WrapTransportError(err)
	}
	conn.SetReadLimit(profile.effectiveMaxEventBytes())
	return conn, nil
}

func responseHeaderValues(headers http.Header) map[string]string {
	values := make(map[string]string, len(headers))
	for name := range headers {
		values[name] = headers.Get(name)
	}
	return values
}

func (c *client) effectiveConnectTimeout() time.Duration {
	if c.connectTimeout > 0 {
		return c.connectTimeout
	}
	return 30 * time.Second
}

func (c *client) effectiveWriteTimeout() time.Duration {
	if c.writeTimeout > 0 {
		return c.writeTimeout
	}
	return defaultWriteTimeout
}

func (c *client) effectiveReadTimeout() time.Duration {
	if c.readTimeout > 0 {
		return c.readTimeout
	}
	return defaultReadIdleTimeout
}

func (c *client) effectiveMaxEventBytes() int64 {
	if c.maxEventBytes > 0 {
		return c.maxEventBytes
	}
	return defaultMaxEventBytes
}

func defaultRetryPolicy() manifest.RetryPolicy {
	return manifest.RetryPolicy{
		MaxAttempts:       2,
		Factor:            1,
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

func retryPolicyAttempts(policy manifest.RetryPolicy) int {
	if policy.MaxAttempts < 1 {
		return 1
	}
	return policy.MaxAttempts
}

func retryCodexRequest(ctx context.Context, policy manifest.RetryPolicy, attempt int, emitted, retryable bool) (bool, error) {
	if !retryable || attempt >= retryPolicyAttempts(policy) || policy.ReplayRequirement == "never" || policy.ReplayRequirement == "before-first-event" && emitted {
		return false, nil
	}
	if err := providertransport.WaitForRetry(ctx, providertransport.RetryDelay(policy, attempt, "")); err != nil {
		return false, err
	}
	return true, nil
}

func abnormalClosureRetryDelay(attempt int) time.Duration {
	delay := abnormalClosureInitialRetryDelay
	for index := 1; index < attempt && delay < abnormalClosureMaxRetryDelay; index++ {
		if delay > abnormalClosureMaxRetryDelay/2 {
			return abnormalClosureMaxRetryDelay
		}
		delay *= 2
	}
	if delay > abnormalClosureMaxRetryDelay {
		return abnormalClosureMaxRetryDelay
	}
	return delay
}

func waitForAbnormalClosureReconnect(ctx context.Context, attempt int) error {
	return providertransport.WaitForRetry(ctx, abnormalClosureRetryDelay(attempt))
}

func codexErrorRetryable(policy manifest.RetryPolicy, mappings []manifest.ErrorMapping, err error) bool {
	if providertransport.RetryOperationError(policy, mappings, err, false) {
		return true
	}
	if policy.TransportErrors {
		var networkError net.Error
		return errors.As(err, &networkError) || websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway)
	}
	return false
}

func codexTerminalProviderError(event *eventFrame, body []byte) *fantasy.ProviderError {
	if event == nil {
		return nil
	}
	switch event.Type {
	case "response.failed", "response.incomplete":
		var responseError *wireError
		if event.Response != nil {
			responseError = event.Response.Error
		}
		return codexProviderErrorWithBody(responseError, "codex response did not complete", body)
	case "response.completed":
		if event.Response != nil && event.Response.Error != nil {
			return codexProviderErrorWithBody(event.Response.Error, "codex response failed", body)
		}
	case "error":
		responseError := event.Error
		if responseError == nil && (event.Code != "" || event.Message != "") {
			responseError = &wireError{Code: event.Code, Message: event.Message}
		}
		return codexProviderErrorWithBody(responseError, "unknown codex error", body)
	}
	return nil
}

func (c *client) startReadPump(conn *websocket.Conn, traceID string) (<-chan websocketReadResult, chan struct{}) {
	results := make(chan websocketReadResult, readPumpBufferSize)
	stop := make(chan struct{})
	go func() {
		for {
			messageType, data, err := conn.ReadMessage()
			cruxlog.TraceWebSocketFrame(traceID, "inbound", c.url, messageType, data, err)
			select {
			case results <- websocketReadResult{data: data, err: err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return results, stop
}

func lockSessionState(ctx context.Context, state *sessionState) error {
	for !state.mu.TryLock() {
		timer := time.NewTimer(stateLockPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (c *client) transportIdentity() string {
	data, _ := json.Marshal(struct {
		UserAgent  string            `json:"user_agent"`
		Originator string            `json:"originator"`
		Version    string            `json:"version"`
		Headers    map[string]string `json:"headers"`
	}{
		UserAgent:  c.userAgent,
		Originator: c.originator,
		Version:    c.version,
		Headers:    c.headers,
	})
	return stableHash(string(data))
}

func (c *client) stream(ctx context.Context, logical *requestFrame, provider, conversationID, purpose string) func(yield func(*eventFrame, error) bool) {
	return c.streamWithProfile(ctx, logical, provider, conversationID, purpose, c.inferenceProfile())
}

func (c *client) streamWithProfile(ctx context.Context, logical *requestFrame, provider, conversationID, purpose string, profile executionProfile) func(yield func(*eventFrame, error) bool) {
	return func(yield func(*eventFrame, error) bool) {
		if profile.requestTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, profile.requestTimeout)
			defer cancel()
		}
		policy := profile.retry
		token := ""
		if c.token != nil {
			token = c.token()
		}
		accountID := c.chatGPTAccountID(token)
		account := accountDiscriminator(accountID, token)
		compatibilityID := compatibilityIdentity(provider, account, conversationID, purpose)
		if conversationID != "" {
			logical.PromptCacheKey = promptCacheKey(provider, account, conversationID, purpose)
		}
		logical.ClientMetadata = requestClientMetadata(compatibilityID, logical.RequestKind)
		requestBudget := c.effectiveRequestBudget()
		bounded, budgetStats, err := fitCodexRequest(logical, requestBudget)
		if err != nil {
			yield(nil, err)
			return
		}
		logical = bounded

		reusable := c.sessionStore != nil && conversationID != ""
		state := &sessionState{}
		if reusable {
			state = c.sessionStore.state(newTransportStateKey(c.url, provider, account, logical.Model, conversationID, purpose, c.transportIdentity()))
		}
		if err := lockSessionState(ctx, state); err != nil {
			yield(nil, err)
			return
		}
		defer state.mu.Unlock()
		defer func() { state.lastUsed = time.Now() }()
		if !reusable {
			defer state.closeLocked()
		}
		if state.conn != nil && (state.token != token || state.accountID != accountID) {
			state.closeLocked()
		}

		forceFull := false
		messageTooBigRetryUsed := false
		abnormalClosureReconnects := 0
		attempt := 0
		for {
			wire, incremental, fallbackReason := state.wireRequestLocked(logical)
			if forceFull {
				state.clearChainLocked()
				wire = fullWireRequest(logical)
				incremental = false
				fallbackReason = "reconnected"
			}
			connectionReused := state.conn != nil
			attemptStarted := false
			if state.conn == nil {
				attempt++
				attemptStarted = true
				traceID := cruxlog.NewWebSocketTraceID()
				conn, err := c.dialWithProfile(ctx, token, accountID, compatibilityID, traceID, profile)
				if err != nil {
					retryable := codexErrorRetryable(policy, profile.errors, err)
					if abnormalClosureReconnects > 0 && retryable {
						abnormalClosureReconnects++
						if waitErr := waitForAbnormalClosureReconnect(ctx, abnormalClosureReconnects); waitErr != nil {
							yield(nil, waitErr)
							return
						}
						forceFull = true
						continue
					}
					retry, waitErr := retryCodexRequest(ctx, policy, attempt, false, retryable)
					if waitErr != nil {
						yield(nil, waitErr)
						return
					}
					if retry {
						forceFull = true
						continue
					}
					yield(nil, err)
					return
				}
				state.conn = conn
				state.readEvents, state.readStop = c.startReadPump(conn, traceID)
				state.traceID = traceID
				state.token = token
				state.accountID = accountID
				state.clearChainLocked()
				wire = fullWireRequest(logical)
				incremental = false
				if fallbackReason == "" || fallbackReason == "no_previous_response" {
					fallbackReason = "new_connection"
				}
			}
			if connectionReused {
				stale := false
			drainStaleReads:
				for {
					select {
					case <-state.readEvents:
						stale = true
					default:
						break drainStaleReads
					}
				}
				if stale {
					state.closeLocked()
					forceFull = true
					continue
				}
			}
			if !attemptStarted {
				attempt++
			}

			logicalRequestBytes, _ := json.Marshal(logical)
			wireRequestBytes, _ := json.Marshal(wire)
			toolSchemaBytes, _ := json.Marshal(logical.Tools)
			slog.Debug("Codex request prepared",
				"session", shortHash(conversationID),
				"connection_reused", connectionReused,
				"incremental", incremental,
				"fallback_reason", fallbackReason,
				"logical_items", len(logical.Input),
				"wire_items", len(wire.Input),
				"logical_request_bytes", len(logicalRequestBytes),
				"wire_request_bytes", len(wireRequestBytes),
				"budget_original_bytes", budgetStats.originalBytes,
				"budget_final_bytes", budgetStats.finalBytes,
				"budget_compressed_images", budgetStats.compressedImages,
				"budget_omitted_images", budgetStats.omittedImages,
				"instruction_bytes", len(logical.Instructions),
				"tool_count", len(logical.Tools),
				"tool_schema_bytes", len(toolSchemaBytes),
				"logical_tool_output_bytes", toolOutputBytes(logical.Input),
				"wire_tool_output_bytes", toolOutputBytes(wire.Input),
				"prompt_cache_key", shortHash(logical.PromptCacheKey),
				"previous_response", wire.PreviousResponseID != "",
			)

			wireData, marshalErr := json.Marshal(wire)
			if marshalErr != nil {
				yield(nil, fmt.Errorf("codex: encode WebSocket request: %w", marshalErr))
				return
			}
			if err := ctx.Err(); err != nil {
				state.closeLocked()
				yield(nil, err)
				return
			}
			conn := state.conn
			conn.SetReadLimit(profile.effectiveMaxEventBytes())
			readEvents := state.readEvents
			done := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					_ = conn.Close()
				case <-done:
				}
			}()
			if c.ownerValidator != nil {
				if err := c.ownerValidator(); err != nil {
					close(done)
					state.closeLocked()
					yield(nil, fmt.Errorf("Codex provider owner changed before WebSocket write: %w", err))
					return
				}
			}
			writeErr := conn.SetWriteDeadline(time.Now().Add(c.effectiveWriteTimeout()))
			if writeErr == nil {
				writeErr = conn.WriteMessage(websocket.TextMessage, wireData)
			}
			if writeErr == nil {
				writeErr = conn.SetWriteDeadline(time.Time{})
			}
			cruxlog.TraceWebSocketFrame(state.traceID, "outbound", c.url, websocket.TextMessage, wireData, writeErr)
			if writeErr != nil {
				close(done)
				state.closeLocked()
				if err := ctx.Err(); err != nil {
					yield(nil, err)
					return
				}
				transportErr := fantasy.WrapTransportError(writeErr)
				if abnormalClosureReconnects > 0 && policy.TransportErrors {
					abnormalClosureReconnects++
					if waitErr := waitForAbnormalClosureReconnect(ctx, abnormalClosureReconnects); waitErr != nil {
						yield(nil, waitErr)
						return
					}
					forceFull = true
					continue
				}
				retry, waitErr := retryCodexRequest(ctx, policy, attempt, false, policy.TransportErrors)
				if waitErr != nil {
					yield(nil, waitErr)
					return
				}
				if retry {
					forceFull = true
					continue
				}
				yield(nil, transportErr)
				return
			}

			receivedEvent := false
			var completedOutput []json.RawMessage
			retry := false
			for {
				if err := ctx.Err(); err != nil {
					close(done)
					state.closeLocked()
					yield(nil, err)
					return
				}
				var result websocketReadResult
				readTimer := time.NewTimer(profile.effectiveReadTimeout())
				select {
				case <-ctx.Done():
					if !readTimer.Stop() {
						<-readTimer.C
					}
					close(done)
					state.closeLocked()
					yield(nil, ctx.Err())
					return
				case result = <-readEvents:
					if !readTimer.Stop() {
						<-readTimer.C
					}
				case <-readTimer.C:
					close(done)
					state.closeLocked()
					idleErr := fantasy.WrapTransportError(fmt.Errorf("codex: idle timeout waiting for WebSocket activity"))
					shouldRetry, waitErr := retryCodexRequest(ctx, policy, attempt, receivedEvent, policy.TransportErrors)
					if waitErr != nil {
						yield(nil, waitErr)
						return
					}
					if shouldRetry {
						forceFull = true
						retry = true
						break
					}
					yield(nil, idleErr)
					return
				}
				if retry {
					break
				}
				data, err := result.data, result.err
				if err != nil {
					close(done)
					state.closeLocked()
					if ctx.Err() != nil {
						yield(nil, ctx.Err())
						return
					}
					if errors.Is(err, websocket.ErrReadLimit) {
						yield(nil, fmt.Errorf("codex: WebSocket event exceeds %d bytes", profile.effectiveMaxEventBytes()))
						return
					}
					if retryErr := classifyCodexCloseError(err); retryErr != nil {
						yield(nil, retryErr)
						return
					}
					if websocket.IsCloseError(err, websocket.CloseAbnormalClosure) && !receivedEvent {
						abnormalClosureReconnects++
						if waitErr := waitForAbnormalClosureReconnect(ctx, abnormalClosureReconnects); waitErr != nil {
							yield(nil, waitErr)
							return
						}
						forceFull = true
						retry = true
						break
					}
					if websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
						if !receivedEvent && !messageTooBigRetryUsed {
							shouldRetry, waitErr := retryCodexRequest(ctx, policy, attempt, false, true)
							if waitErr != nil {
								yield(nil, waitErr)
								return
							}
							if shouldRetry {
								retryLogical, retryStats, retryErr := fitCodexRequestTo(logical, requestBudget, requestBudget.retryRequestBytes)
								if retryErr != nil {
									yield(nil, fmt.Errorf("codex: server rejected the request as too large: %w", retryErr))
									return
								}
								messageTooBigRetryUsed = true
								logical = retryLogical
								budgetStats = retryStats
								forceFull = true
								retry = true
								break
							}
						}
						yield(nil, fmt.Errorf("codex: server rejected the request as too large after the declared retry budget; compact the session before retrying: %w", err))
						return
					}
					if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						shouldRetry, waitErr := retryCodexRequest(ctx, policy, attempt, receivedEvent, policy.UnexpectedEOF)
						if waitErr != nil {
							yield(nil, waitErr)
							return
						}
						if shouldRetry {
							forceFull = true
							retry = true
							break
						}
						return
					}
					transportErr := fantasy.WrapTransportError(err)
					shouldRetry, waitErr := retryCodexRequest(ctx, policy, attempt, receivedEvent, policy.TransportErrors)
					if waitErr != nil {
						yield(nil, waitErr)
						return
					}
					if shouldRetry {
						forceFull = true
						retry = true
						break
					}
					yield(nil, transportErr)
					return
				}
				var event eventFrame
				if err := json.Unmarshal(data, &event); err != nil {
					close(done)
					state.closeLocked()
					yield(nil, fmt.Errorf("codex: decode WebSocket event: %w", err))
					return
				}
				if event.Type == "" {
					close(done)
					state.closeLocked()
					yield(nil, fmt.Errorf("codex: WebSocket event missing type"))
					return
				}
				if providerErr := codexTerminalProviderError(&event, data); providerErr != nil {
					providertransport.MapError(profile.errors, providerErr)
					shouldRetry, waitErr := retryCodexRequest(ctx, policy, attempt, receivedEvent, providertransport.RetryOperationError(policy, profile.errors, providerErr, receivedEvent))
					if waitErr != nil {
						close(done)
						state.closeLocked()
						yield(nil, waitErr)
						return
					}
					if shouldRetry {
						close(done)
						state.closeLocked()
						forceFull = true
						retry = true
						break
					}
					event.mappedError = providerErr
				}
				receivedEvent = true
				if event.Type == "response.output_item.done" && len(event.Item) > 0 {
					completedOutput = append(completedOutput, append(json.RawMessage(nil), event.Item...))
				}
				if event.Type == "response.completed" {
					if event.Response == nil {
						close(done)
						state.closeLocked()
						yield(nil, fmt.Errorf("codex: completed response missing payload"))
						return
					}
					if len(event.Response.Output) == 0 && len(completedOutput) > 0 {
						response := *event.Response
						response.Output = completedOutput
						event.Response = &response
					}
					if event.Response.Error != nil || !state.commitLocked(logical, event.Response) {
						state.clearChainLocked()
					}
				}
				if !yield(&event, nil) {
					close(done)
					if event.Type != "response.completed" {
						state.closeLocked()
					}
					return
				}
				switch event.Type {
				case "response.completed":
					close(done)
					return
				case "response.failed", "response.incomplete", "error":
					close(done)
					state.closeLocked()
					return
				}
			}
			if retry {
				continue
			}
		}
	}
}

func classifyCodexCloseError(err error) error {
	closeErr, ok := err.(*websocket.CloseError)
	if !ok {
		return nil
	}
	switch closeErr.Text {
	case fantasy.ServerOverloadMessage:
		return fantasy.NewServerOverloadError()
	case fantasy.ConnectionLimitMessage:
		return fantasy.NewConnectionLimitError()
	default:
		return nil
	}
}

func requestClientMetadata(compatibilityID, requestKind string) map[string]string {
	windowID := compatibilityID + ":0"
	metadata := map[string]string{
		"x-codex-installation-id": installationID,
		"session_id":              compatibilityID,
		"thread_id":               compatibilityID,
		"x-codex-window-id":       windowID,
	}
	turnMetadata := map[string]string{
		"installation_id": installationID,
		"session_id":      compatibilityID,
		"thread_id":       compatibilityID,
		"window_id":       windowID,
	}
	if requestKind != "" {
		turnMetadata["request_kind"] = requestKind
	}
	if encoded, err := json.Marshal(turnMetadata); err == nil {
		metadata["x-codex-turn-metadata"] = string(encoded)
	}
	return metadata
}

func appendHeaderValue(current, value string) string {
	for feature := range strings.SplitSeq(current, ",") {
		if strings.TrimSpace(feature) == value {
			return current
		}
	}
	if strings.TrimSpace(current) == "" {
		return value
	}
	return current + "," + value
}

// chatGPTAccountID follows Codex's selected-account behavior. The account id
// stored alongside the OAuth credentials is authoritative; JWT extraction is
// retained for callers that do not use the shared account store.
func (c *client) chatGPTAccountID(token string) string {
	if c.accountID != nil {
		if accountID := c.accountID(); accountID != "" {
			return accountID
		}
	}
	return accountIDFromJWT(token)
}

// accountIDFromJWT extracts the ChatGPT account id from the JWT access
// token. Returns "" on any failure.
func accountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}
