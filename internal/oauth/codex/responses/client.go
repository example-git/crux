package responses

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	cruxlog "github.com/example-git/crux/internal/log"
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
	openaiBeta                    = "responses_websockets=2026-02-06"
	remoteCompactionV2BetaFeature = "remote_compaction_v2"
	maxAbnormalClosureReconnects  = 3
	abnormalReconnectBaseDelay    = 100 * time.Millisecond
)

// installationID is a stable per-process Codex installation identifier.
var installationID = uuid.NewString()

type client struct {
	url          string
	token        TokenSource
	accountID    AccountIDSource
	userAgent    string
	originator   string
	version      string
	headers      map[string]string
	sessionStore *SessionStore
}

// dial opens the WebSocket connection with the Codex identity headers.
func (c *client) dial(ctx context.Context, token, accountID, compatibilityID, traceID string) (*websocket.Conn, error) {
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

	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	cruxlog.TraceWebSocketHandshake(traceID, "outbound", c.url, header, 0, 0, nil)
	started := time.Now()
	conn, resp, err := dialer.DialContext(ctx, c.url, header)
	statusCode := 0
	responseHeaders := http.Header(nil)
	if resp != nil {
		statusCode = resp.StatusCode
		responseHeaders = resp.Header.Clone()
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	cruxlog.TraceWebSocketHandshake(traceID, "inbound", c.url, responseHeaders, statusCode, time.Since(started), err)
	if err != nil {
		if resp != nil {
			return nil, &fantasy.ProviderError{
				Title:      "connection failed",
				Message:    fmt.Sprintf("codex: WebSocket handshake failed: %v", err),
				StatusCode: resp.StatusCode,
			}
		}
		return nil, fantasy.WrapTransportError(err)
	}
	return conn, nil
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
	return func(yield func(*eventFrame, error) bool) {
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
		bounded, budgetStats, err := fitCodexRequest(logical)
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
		state.mu.Lock()
		defer state.mu.Unlock()
		defer func() { state.lastUsed = time.Now() }()
		if !reusable {
			defer state.closeLocked()
		}
		if state.conn != nil && (state.token != token || state.accountID != accountID) {
			state.closeLocked()
		}

		forceFull := false
		standardRetryUsed := false
		messageTooBigRetryUsed := false
		abnormalReconnects := 0
		reconnectingAfterAbnormal := false
		for {
			wire, incremental, fallbackReason := state.wireRequestLocked(logical)
			if forceFull {
				state.clearChainLocked()
				wire = fullWireRequest(logical)
				incremental = false
				fallbackReason = "reconnected"
			}
			connectionReused := state.conn != nil
			if state.conn == nil {
				traceID := cruxlog.NewWebSocketTraceID()
				conn, err := c.dial(ctx, token, accountID, compatibilityID, traceID)
				if err != nil {
					if reconnectingAfterAbnormal && abnormalReconnects < maxAbnormalClosureReconnects {
						abnormalReconnects++
						if waitErr := waitForAbnormalReconnect(ctx, abnormalReconnects); waitErr != nil {
							yield(nil, waitErr)
							return
						}
						continue
					}
					yield(nil, err)
					return
				}
				state.conn = conn
				state.traceID = traceID
				state.token = token
				state.accountID = accountID
				reconnectingAfterAbnormal = false
				state.clearChainLocked()
				wire = fullWireRequest(logical)
				incremental = false
				if fallbackReason == "" || fallbackReason == "no_previous_response" {
					fallbackReason = "new_connection"
				}
			}

			wire.ClientMetadata = requestClientMetadata(compatibilityID, logical.RequestKind)
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
			writeErr := state.conn.WriteMessage(websocket.TextMessage, wireData)
			cruxlog.TraceWebSocketFrame(state.traceID, "outbound", c.url, websocket.TextMessage, wireData, writeErr)
			if writeErr != nil {
				state.closeLocked()
				if !standardRetryUsed {
					standardRetryUsed = true
					forceFull = true
					continue
				}
				yield(nil, fantasy.WrapTransportError(writeErr))
				return
			}

			conn := state.conn
			done := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					_ = conn.Close()
				case <-done:
				}
			}()

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
				messageType, data, err := conn.ReadMessage()
				cruxlog.TraceWebSocketFrame(state.traceID, "inbound", c.url, messageType, data, err)
				if err != nil {
					close(done)
					state.closeLocked()
					if ctx.Err() != nil {
						yield(nil, ctx.Err())
						return
					}
					if websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
						if !receivedEvent && !messageTooBigRetryUsed {
							retryLogical, retryStats, retryErr := fitCodexRequestTo(logical, retryCodexRequestBytes)
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
						yield(nil, fmt.Errorf("codex: server rejected the request as too large after a %d-byte full-replay retry; compact the session before retrying: %w", retryCodexRequestBytes, err))
						return
					}
					if !receivedEvent && websocket.IsCloseError(err, websocket.CloseAbnormalClosure) && abnormalReconnects < maxAbnormalClosureReconnects {
						abnormalReconnects++
						reconnectingAfterAbnormal = true
						forceFull = true
						if waitErr := waitForAbnormalReconnect(ctx, abnormalReconnects); waitErr != nil {
							yield(nil, waitErr)
							return
						}
						retry = true
						break
					}
					if websocket.IsCloseError(err, websocket.CloseAbnormalClosure) {
						yield(nil, fantasy.WrapTransportError(err))
						return
					}
					if !receivedEvent && !standardRetryUsed {
						standardRetryUsed = true
						forceFull = true
						retry = true
						break
					}
					if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						return
					}
					yield(nil, fantasy.WrapTransportError(err))
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

func waitForAbnormalReconnect(ctx context.Context, attempt int) error {
	delay := abnormalReconnectBaseDelay * time.Duration(1<<max(attempt-1, 0))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
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
