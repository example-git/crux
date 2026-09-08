package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestClientCodexContinuationBelongsToAcceptedGeneration(t *testing.T) {
	type request struct {
		frame      map[string]any
		connection int32
		sessionID  string
	}
	var mu sync.Mutex
	var requests []request
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connection := connections.Add(1)
		for {
			var frame map[string]any
			if conn.ReadJSON(&frame) != nil {
				return
			}
			mu.Lock()
			requests = append(requests, request{frame: frame, connection: connection, sessionID: r.Header.Get("session_id")})
			id := fmt.Sprintf("resp_%d", len(requests))
			mu.Unlock()
			item := map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "answer"}}}
			if conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "answer"}) != nil {
				return
			}
			if conn.WriteJSON(map[string]any{"type": "response.output_item.done", "item": item}) != nil {
				return
			}
			if conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "output": []any{}}}) != nil {
				return
			}
		}
	}))
	defer host.Close()
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("codex")
	require.True(t, ok)
	selected := config.SelectedModel{Provider: "codex", Model: "fixture"}
	proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers:   []config.RemoteProviderDefinition{{Config: config.ProviderConfig{ID: "codex", Type: catalog.TypeOpenAICompat, BaseURL: "wss://fixture.invalid/responses", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex}, Models: []catalog.Model{{ID: "fixture", Name: "Fixture"}}}}},
		Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: selected, config.SelectedModelTypeSmall: selected},
		Credentials: []config.RemoteCredentialBinding{{Owner: registration.Owner(), Generation: 1, Account: &accounts.Entry{ID: "same-account", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Raw: json.RawMessage(`{"account_id":"synthetic-account-id"}`)}}},
	}
	sealClientResponsesProposal(t, &proposal)
	principal := strings.Repeat("a", 64)
	compile := func(proposal config.RemoteRuntimeProposal, principal string) *config.ConfigStore {
		root := t.TempDir()
		store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, principal, env.NewFromMap(map[string]string{}))
		require.NoError(t, err)
		return store
	}
	store := compile(proposal, principal)
	coord := &coordinator{cfg: store, codexSessions: codexresponses.NewSessionStore()}
	defer coord.codexSessions.Close()
	build := func(snapshot config.RuntimeSnapshot) fantasy.LanguageModel {
		providerConfig, ok := snapshot.Config().Providers.Get("codex")
		require.True(t, ok)
		// Exercise the real coordinator constructor and adapter on loopback WS.
		// The compiler-admitted authority remains WSS; this fixture does not
		// claim to test remote transport admission or certificate validation.
		provider, err := coord.buildCodexProvider(snapshot, registration, strings.Replace(host.URL, "http://", "ws://", 1), providerConfig.APIKey, nil, func() error { return nil })
		require.NoError(t, err)
		model, err := provider.LanguageModel(t.Context(), "fixture")
		require.NoError(t, err)
		return model
	}
	consume := func(model fantasy.LanguageModel, call fantasy.Call) {
		response, err := model.Generate(t.Context(), call)
		require.NoError(t, err)
		require.Contains(t, response.Content.Text(), "answer")
	}
	first := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("one")}, Headers: map[string]string{"x-session-id": "same-session", "x-request-purpose": "conversation"}}
	next := func(call fantasy.Call, text string) fantasy.Call {
		call.Prompt = append(append(fantasy.Prompt{}, call.Prompt...), fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer"}}}, fantasy.NewUserMessage(text))
		return call
	}
	second := next(first, "two")
	third := next(second, "three")
	fourth := next(third, "four")
	old := build(store.RuntimeSnapshot())
	consume(old, first)
	consume(build(store.RuntimeSnapshot()), second)
	proposal.Revision = 2
	proposal.Credentials[0].Generation = 2
	sealClientResponsesProposal(t, &proposal)
	_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
	require.NoError(t, err)
	consume(build(store.RuntimeSnapshot()), third)
	consume(old, third)
	consume(build(compile(proposal, strings.Repeat("b", 64)).RuntimeSnapshot()), fourth)
	proposal.Providers[0].Config.Name = "Changed content"
	sealClientResponsesProposal(t, &proposal)
	consume(build(compile(proposal, principal).RuntimeSnapshot()), fourth)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, requests, 6)
	require.Equal(t, "resp_1", requests[1].frame["previous_response_id"])
	require.NotContains(t, requests[2].frame, "previous_response_id", "new accepted revision must start its own chain")
	require.Len(t, requests[2].frame["input"], 5)
	require.NotEqual(t, requests[0].connection, requests[2].connection)
	require.NotEqual(t, requests[0].sessionID, requests[2].sessionID)
	require.NotEqual(t, requests[0].frame["prompt_cache_key"], requests[2].frame["prompt_cache_key"])
	require.Equal(t, "resp_2", requests[3].frame["previous_response_id"])
	require.Equal(t, requests[0].connection, requests[3].connection, "new generation must not retire the old capture's connection")
	require.Len(t, requests[3].frame["input"], 1)
	require.Equal(t, requests[0].frame["prompt_cache_key"], requests[3].frame["prompt_cache_key"])
	for _, index := range []int{4, 5} {
		require.NotContains(t, requests[index].frame, "previous_response_id")
		require.Len(t, requests[index].frame["input"], 7)
		require.NotEqual(t, requests[2].connection, requests[index].connection)
		require.NotEqual(t, requests[2].sessionID, requests[index].sessionID)
		require.NotEqual(t, requests[2].frame["prompt_cache_key"], requests[index].frame["prompt_cache_key"])
	}
	for _, request := range requests {
		metadata, ok := request.frame["client_metadata"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, request.sessionID, metadata["session_id"])
	}
}
