package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const sessionStoreIdleTTL = 30 * time.Minute

// SessionStore owns reusable Codex WebSocket and response-chain state. A store
// must be scoped to one Crux workspace/coordinator and shared by rebuilt Codex
// provider instances so conversation state survives model refreshes.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[transportStateKey]*sessionState
}

type sessionState struct {
	mu sync.Mutex

	conn      *websocket.Conn
	traceID   string
	token     string
	accountID string
	lastUsed  time.Time
	chain     *responseChain
}

type responseChain struct {
	properties        requestProperties
	sourceRepresented []inputItem
	dynamicContext    string
	responseID        string
}

type requestProperties struct {
	Model             string
	Instructions      string
	Tools             []wireTool
	ToolChoice        string
	ParallelToolCalls bool
	Reasoning         *wireReasoning
	Stream            bool
	Include           []string
	Text              *wireTextFormat
	PromptCacheKey    string
	Store             bool
}

// NewSessionStore creates an isolated Codex transport-state owner.
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[transportStateKey]*sessionState)}
}

func (s *SessionStore) state(key transportStateKey) *sessionState {
	if s == nil {
		return &sessionState{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[transportStateKey]*sessionState)
	}
	now := time.Now()
	for existingKey, state := range s.sessions {
		if !state.mu.TryLock() {
			continue
		}
		expired := now.Sub(state.lastUsed) > sessionStoreIdleTTL
		identityReplaced := existingKey != key && existingKey.sameConversationPurpose(key)
		if expired || identityReplaced {
			state.closeLocked()
			delete(s.sessions, existingKey)
		}
		state.mu.Unlock()
	}
	if state := s.sessions[key]; state != nil {
		return state
	}
	state := &sessionState{lastUsed: now}
	s.sessions[key] = state
	return state
}

// Close closes every reusable Codex connection and clears all chain state.
func (s *SessionStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	states := make([]*sessionState, 0, len(s.sessions))
	for _, state := range s.sessions {
		states = append(states, state)
	}
	s.sessions = make(map[transportStateKey]*sessionState)
	s.mu.Unlock()
	for _, state := range states {
		state.mu.Lock()
		state.closeLocked()
		state.mu.Unlock()
	}
}

func (s *SessionStore) clearChain(key transportStateKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	state := s.sessions[key]
	s.mu.Unlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	state.clearChainLocked()
	state.lastUsed = time.Now()
	state.mu.Unlock()
}

func (s *SessionStore) resetConversation(endpoint, provider, account, conversationID string) {
	if s == nil || conversationID == "" {
		return
	}
	s.mu.Lock()
	states := make([]*sessionState, 0)
	for key, state := range s.sessions {
		if key.endpoint == endpoint && key.provider == provider && key.account == account && key.conversation == conversationID {
			states = append(states, state)
		}
	}
	s.mu.Unlock()
	for _, state := range states {
		state.mu.Lock()
		state.closeLocked()
		state.lastUsed = time.Now()
		state.mu.Unlock()
	}
}

func (s *sessionState) closeLocked() {
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.conn = nil
	s.traceID = ""
	s.token = ""
	s.accountID = ""
	s.chain = nil
}

func (s *sessionState) clearChainLocked() {
	s.chain = nil
}

func propertiesOf(frame *requestFrame) requestProperties {
	return requestProperties{
		Model:             frame.Model,
		Instructions:      frame.Instructions,
		Tools:             cloneWireTools(frame.Tools),
		ToolChoice:        frame.ToolChoice,
		ParallelToolCalls: frame.ParallelToolCalls,
		Reasoning:         cloneJSONValue(frame.Reasoning),
		Stream:            frame.Stream,
		Include:           append([]string(nil), frame.Include...),
		Text:              cloneJSONValue(frame.Text),
		PromptCacheKey:    frame.PromptCacheKey,
		Store:             frame.Store,
	}
}

func (s *sessionState) wireRequestLocked(logical *requestFrame) (*requestFrame, bool, string) {
	wire := fullWireRequest(logical)
	if s.chain == nil {
		return wire, false, "no_previous_response"
	}
	if s.chain.responseID == "" {
		s.clearChainLocked()
		return wire, false, "empty_previous_response"
	}
	if !reflect.DeepEqual(s.chain.properties, propertiesOf(logical)) {
		s.clearChainLocked()
		return wire, false, "request_properties_changed"
	}
	if !hasInputPrefix(logical.Input, s.chain.sourceRepresented) {
		s.clearChainLocked()
		return wire, false, "history_not_append_only"
	}
	wire.PreviousResponseID = s.chain.responseID
	wire.Input = cloneInputItems(logical.Input[len(s.chain.sourceRepresented):])
	if logical.DynamicContext != "" && logical.DynamicContext != s.chain.dynamicContext {
		wire.Input = append([]inputItem{dynamicEnvironmentItem(logical.DynamicContext)}, wire.Input...)
	}
	return wire, true, ""
}

func (s *sessionState) commitLocked(logical *requestFrame, response *wireResponse) bool {
	if response == nil || response.ID == "" {
		s.clearChainLocked()
		return false
	}
	output, ok := responseInputItems(response.Output)
	if !ok {
		s.clearChainLocked()
		return false
	}
	sourceRepresented := make([]inputItem, 0, len(logical.Input)+len(output))
	sourceRepresented = append(sourceRepresented, cloneInputItems(logical.Input)...)
	sourceRepresented = append(sourceRepresented, output...)
	s.chain = &responseChain{
		properties:        propertiesOf(logical),
		sourceRepresented: sourceRepresented,
		dynamicContext:    logical.DynamicContext,
		responseID:        response.ID,
	}
	return true
}

func fullWireRequest(logical *requestFrame) *requestFrame {
	wire := cloneRequestFrame(logical)
	wire.PreviousResponseID = ""
	if logical.DynamicContext != "" {
		wire.Input = append([]inputItem{dynamicEnvironmentItem(logical.DynamicContext)}, wire.Input...)
	}
	return wire
}

func hasInputPrefix(input, prefix []inputItem) bool {
	if len(input) < len(prefix) {
		return false
	}
	return reflect.DeepEqual(input[:len(prefix)], prefix)
}

func responseInputItems(rawItems []json.RawMessage) ([]inputItem, bool) {
	items := make([]inputItem, 0, len(rawItems))
	for _, raw := range rawItems {
		var item inputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, false
		}
		switch item.Type {
		case "message":
			item.ID = ""
		case "reasoning", "function_call", "compaction":
		default:
			return nil, false
		}
		items = append(items, item)
	}
	return items, true
}

func cloneRequestFrame(frame *requestFrame) *requestFrame {
	if frame == nil {
		return nil
	}
	clone := *frame
	clone.Input = cloneInputItems(frame.Input)
	clone.Tools = cloneWireTools(frame.Tools)
	clone.Reasoning = cloneJSONValue(frame.Reasoning)
	clone.Include = append([]string(nil), frame.Include...)
	clone.Text = cloneJSONValue(frame.Text)
	clone.ClientMetadata = cloneStringMap(frame.ClientMetadata)
	return &clone
}

func cloneInputItems(items []inputItem) []inputItem {
	if items == nil {
		return nil
	}
	clone := make([]inputItem, len(items))
	for i, item := range items {
		clone[i] = item
		clone[i].Content = append([]messageContent(nil), item.Content...)
		clone[i].Summary = append(json.RawMessage(nil), item.Summary...)
		if item.Output != nil {
			output := *item.Output
			clone[i].Output = &output
		}
	}
	return clone
}

func cloneWireTools(tools []wireTool) []wireTool {
	return cloneJSONValue(tools)
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func cloneJSONValue[T any](value T) T {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var clone T
	if err := json.Unmarshal(data, &clone); err != nil {
		return value
	}
	return clone
}

type transportStateKey struct {
	endpoint     string
	provider     string
	account      string
	model        string
	conversation string
	purpose      string
	transport    string
}

func (k transportStateKey) sameConversationPurpose(other transportStateKey) bool {
	return k.conversation == other.conversation && k.purpose == other.purpose
}

func newTransportStateKey(endpoint, provider, account, modelID, conversationID, purpose, transportIdentity string) transportStateKey {
	return transportStateKey{
		endpoint:     endpoint,
		provider:     provider,
		account:      account,
		model:        modelID,
		conversation: conversationID,
		purpose:      purpose,
		transport:    transportIdentity,
	}
}

func accountDiscriminator(accountID, token string) string {
	if accountID != "" {
		return "account:" + stableHash(accountID)
	}
	if token != "" {
		return "credential:" + stableHash(token)
	}
	return "anonymous"
}

func promptCacheKey(provider, account, conversationID, purpose string) string {
	return stableHash("crux-codex-cache\x00" + provider + "\x00" + account + "\x00" + conversationID + "\x00" + purpose)
}

func compatibilityIdentity(provider, account, conversationID, purpose string) string {
	return stableHash("crux-codex-compatibility\x00" + provider + "\x00" + account + "\x00" + conversationID + "\x00" + purpose)
}

func stableHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func shortHash(value string) string {
	if value == "" {
		return ""
	}
	return stableHash(value)[:12]
}

func toolOutputBytes(items []inputItem) int {
	total := 0
	for _, item := range items {
		if item.Output != nil {
			total += len(*item.Output)
		}
	}
	return total
}
