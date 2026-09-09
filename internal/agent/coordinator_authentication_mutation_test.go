package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type coordinatorAuthenticationFixture struct {
	store         *config.ConfigStore
	coordinator   *coordinator
	environment   fakeEnv
	owner         providerregistry.RegistrationOwner
	first, second accounts.Entry
	marker, scope string
}

func newCoordinatorAuthenticationFixture(t *testing.T, providerID, endpoint string, disabled bool) coordinatorAuthenticationFixture {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"),
		"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"),
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfileIntegrated),
		"CRUX_DISABLE_AUTO_MEMORY": "true", "AUTH_LITERAL": "must-not-expand",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	for _, directory := range []string{"config", "data", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, directory), 0o700))
	}
	registration, ok := integratedRegistration(t, providerID)
	require.True(t, ok)
	marker := filepath.Join(root, "token-must-not-execute")
	first := accounts.Entry{ID: "account-a", AccessToken: "synthetic-account-a", RefreshToken: "synthetic-refresh-a", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Raw: json.RawMessage(`{"account_id":"account-metadata-a"}`)}
	second := accounts.Entry{ID: "account-b", AccessToken: "literal-$(touch '" + marker + "')-$AUTH_LITERAL", RefreshToken: "synthetic-refresh-b", ExpiresAt: first.ExpiresAt, Raw: json.RawMessage(`{"account_id":"account-metadata-b"}`)}
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, first))
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), registration.AccountNamespace, second))
	selectedProvider := providerID
	providers := map[string]any{providerID: map[string]any{
		"base_url": endpoint, "disable": disabled,
		"owner":         map[string]any{"type": config.ProviderOwnerCore, "construction": registration.Construction},
		"extra_headers": map[string]string{"X-Fixture-Keep": "preserved"},
		"models": []map[string]any{
			{"id": "fixture-main", "name": "Main", "context_window": 32000, "default_max_tokens": 128},
			{"id": "fixture-small", "name": "Small", "context_window": 16000, "default_max_tokens": 64},
		},
	}}
	if disabled {
		selectedProvider = "unrelated"
		providers[selectedProvider] = map[string]any{"type": "openai-compat", "base_url": endpoint, "api_key": "unrelated-retained-key",
			"models": []map[string]any{{"id": "fixture-main", "context_window": 32000}, {"id": "fixture-small", "context_window": 16000}}}
	}
	source, err := json.Marshal(map[string]any{
		"providers": providers,
		"models": map[string]any{
			"large": map[string]any{"provider": selectedProvider, "model": "fixture-main", "max_tokens": 91, "temperature": 0.25},
			"small": map[string]any{"provider": selectedProvider, "model": "fixture-small", "max_tokens": 37},
		},
		"options": map[string]any{"disable_auto_summarize": true, "notifications": "disabled"},
		"tools":   map[string]any{"codebase_search": map[string]any{"enabled": false}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), source, 0o600))
	scope := filepath.Join(root, "workspace", "crux.json")
	credentialSource, err := json.Marshal(map[string]any{"providers": map[string]any{providerID: map[string]any{"api_key": first.AccessToken, "oauth": first.Token()}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(scope, credentialSource, 0o600))
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner(providerID)
	require.True(t, ok)
	require.Equal(t, registration.Owner(), owner)
	environment := testEnv(t)
	built, err := NewCoordinator(t.Context(), CoordinatorOptions{
		Config: store, Sessions: environment.sessions, Messages: environment.messages,
		Permissions: environment.permissions, History: environment.history, FileTracker: *environment.filetracker,
	})
	require.NoError(t, err)
	coord := built.(*coordinator)
	t.Cleanup(coord.Close)
	require.NoError(t, coord.readyWg.Wait())
	return coordinatorAuthenticationFixture{store, coord, environment, owner, first, second, marker, scope}
}

func (f coordinatorAuthenticationFixture) capture(t *testing.T) config.AuthenticationCapture {
	t.Helper()
	capture, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	return capture
}

func (f coordinatorAuthenticationFixture) session(t *testing.T, title string) string {
	t.Helper()
	session, err := f.environment.sessions.Create(t.Context(), title)
	require.NoError(t, err)
	// A prior user turn prevents the unrelated automatic first-turn title
	// goroutine. Title generation is exercised explicitly and synchronously.
	_, err = f.environment.messages.Create(t.Context(), session.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "previous user turn"}}})
	require.NoError(t, err)
	return session.ID
}

func (f coordinatorAuthenticationFixture) task(t *testing.T, ctx context.Context, runtime InstalledRuntime, parent, id string) (fantasy.ToolResponse, error) {
	t.Helper()
	for _, tool := range runtime.Tools {
		if tool.Info().Name == AgentToolName {
			ctx := context.WithValue(ctx, tools.SessionIDContextKey, parent)
			ctx = context.WithValue(ctx, tools.MessageIDContextKey, "parent-message")
			return tool.Run(ctx, fantasy.ToolCall{ID: id, Name: AgentToolName, Input: `{"prompt":"generic task after switch","subagent_type":"task"}`})
		}
	}
	t.Fatal("the coordinator did not install its generic task tool")
	return fantasy.ToolResponse{}, nil
}

// This is the actual load -> coordinator -> scoped mutation -> HTTP path. In
// particular no test replaces the runtime preparer or publishes a test store.
func TestCoordinatorAuthenticationMutationCopilotHTTPS(t *testing.T) {
	type request struct{ authorization, model, body, initiator string }
	var mu sync.Mutex
	var requests []request
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce, enteredOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHeld()
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		var payload struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.Unmarshal(body, &payload) != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, request{r.Header.Get("Authorization"), payload.Model, string(body), r.Header.Get("X-Initiator")})
		mu.Unlock()
		if strings.Contains(string(body), "hold-admitted-account-a") {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		if !payload.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"fixture","object":"chat.completion","model":%q,"choices":[{"message":{"role":"assistant","content":"accepted response"},"finish_reason":"stop"}]}`, payload.Model)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"accepted response\"},\"finish_reason\":null}]}\n\n", payload.Model)
		fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", payload.Model)
	}))
	t.Cleanup(host.Close)
	oldTransport, oldClient := http.DefaultTransport, http.DefaultClient
	http.DefaultTransport, http.DefaultClient = host.Client().Transport, host.Client()
	t.Cleanup(func() { http.DefaultTransport, http.DefaultClient = oldTransport, oldClient })
	observed := func() []request { mu.Lock(); defer mu.Unlock(); return slices.Clone(requests) }
	f := newCoordinatorAuthenticationFixture(t, "copilot", host.URL+"/v1", false)
	selected := f.store.RuntimeSnapshot().AgentModelState()
	before := f.capture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	result := make(chan error, 1)
	oldSession := f.session(t, "Admitted account A")
	go func() {
		_, err := f.coordinator.Run(ctx, oldSession, "hold-admitted-account-a")
		result <- err
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("foreground request did not reach HTTPS: %v", err)
	case <-ctx.Done():
		t.Fatal("foreground request did not reach HTTPS")
	}
	initial := f.coordinator.currentAgent.Runtime()
	switched, err := f.store.SwitchAuthenticationAccount(ctx, config.ScopeWorkspace, before, f.owner, f.second.ID)
	require.NoError(t, err)
	require.True(t, switched.AccountsSaved && switched.ConfigSaved && switched.RuntimePublished)
	require.False(t, switched.AccountRefreshed)
	require.Equal(t, selected, f.store.RuntimeSnapshot().AgentModelState())
	require.Same(t, f.store.Config(), f.coordinator.currentAgent.Runtime().Snapshot.Config())
	require.Equal(t, "Bearer "+f.first.AccessToken, observed()[0].authorization)
	releaseHeld()
	select {
	case err := <-result:
		require.NoError(t, err, "already admitted account-A request must finish across switch")
	case <-ctx.Done():
		t.Fatal("admitted foreground request did not finish")
	}
	current := f.coordinator.currentAgent.Runtime()
	newSession := f.session(t, "Account B")
	_, err = f.coordinator.Run(ctx, newSession, "new foreground account B")
	require.NoError(t, err)
	text, err := f.coordinator.currentAgent.GenerateMemory(ctx, "memory_extraction", "memory account B", 32)
	require.NoError(t, err)
	require.Equal(t, "accepted response", text)
	f.coordinator.GenerateTitle(ctx, newSession, "title account B")
	response, err := f.task(t, ctx, current, newSession, "generic-task-b")
	require.NoError(t, err)
	require.False(t, response.IsError, response.Content)
	require.Contains(t, response.Content, "accepted response")
	seen := observed()
	require.Len(t, seen, 5)
	for _, request := range seen[1:] {
		require.Equal(t, "Bearer "+f.second.AccessToken, request.authorization)
	}
	require.Equal(t, []string{"fixture-main", "fixture-main", "fixture-small", "fixture-small", "fixture-main"}, []string{seen[0].model, seen[1].model, seen[2].model, seen[3].model, seen[4].model})
	require.Equal(t, "agent", seen[4].initiator, "generic task must use the integrated subagent transport")
	require.NoFileExists(t, f.marker, "OAuth token bytes must never execute as a shell expression")
	active, err := accounts.Active(ctx, f.owner.AccountNamespace)
	require.NoError(t, err)
	require.Equal(t, f.second.ID, active.ID)
	persisted, err := os.ReadFile(f.scope)
	require.NoError(t, err)
	require.Contains(t, string(persisted), f.second.AccessToken)

	loggedOut, err := f.store.LogoutAuthentication(ctx, config.ScopeWorkspace, f.capture(t), f.owner)
	require.NoError(t, err)
	require.True(t, loggedOut.AccountsSaved && loggedOut.ConfigSaved && loggedOut.RuntimePublished)
	require.Equal(t, selected, f.store.RuntimeSnapshot().AgentModelState())
	require.Same(t, f.store.Config(), f.coordinator.currentAgent.Runtime().Snapshot.Config())
	for _, retained := range []InstalledRuntime{initial, current, f.coordinator.currentAgent.Runtime()} {
		for _, model := range []Model{retained.LargeModel, retained.SmallModel} {
			_, err = model.Model.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("denied after logout")}})
			require.ErrorIs(t, err, config.ErrAuthenticationRevoked)
		}
	}
	_, err = f.coordinator.Run(ctx, newSession, "denied foreground")
	require.ErrorIs(t, err, config.ErrAuthenticationRevoked)
	_, err = f.coordinator.currentAgent.GenerateMemory(ctx, "memory_extraction", "denied memory", 32)
	require.ErrorIs(t, err, config.ErrAuthenticationRevoked)
	f.coordinator.GenerateTitle(ctx, newSession, "denied title")
	for i, retained := range []InstalledRuntime{initial, current, f.coordinator.currentAgent.Runtime()} {
		deniedCtx, stop := context.WithTimeout(t.Context(), time.Second)
		response, err = f.task(t, deniedCtx, retained, newSession, fmt.Sprintf("denied-task-%d", i))
		contextErr := deniedCtx.Err()
		stop()
		require.NoError(t, err)
		require.NoError(t, contextErr, "local denial must return before network retry backoff")
		require.True(t, response.IsError)
		require.Contains(t, response.Content, config.ErrAuthenticationRevoked.Error())
	}
	require.Equal(t, seen, observed(), "logout must deny fresh and retained calls before any additional HTTP request")
	active, err = accounts.Active(ctx, f.owner.AccountNamespace)
	require.NoError(t, err)
	require.Nil(t, active)
	require.NoFileExists(t, f.marker)
}

// Codex is a separate native Responses/WebSocket path. This disposable loopback
// server verifies account metadata on real handshakes, not Copilot behavior or
// TLS certificate policy (the native dialer has no test-root injection seam).
func TestCoordinatorAuthenticationMutationCodexCapturedMetadata(t *testing.T) {
	var mu sync.Mutex
	var credentials, accountIDs []string
	var frames int
	observed := func() ([]string, []string, int) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(credentials), slices.Clone(accountIDs), frames
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		mu.Lock()
		credentials = append(credentials, r.Header.Get("Authorization"))
		accountIDs = append(accountIDs, r.Header.Get("Chatgpt-Account-Id"))
		mu.Unlock()
		for {
			var frame json.RawMessage
			if connection.ReadJSON(&frame) != nil {
				return
			}
			mu.Lock()
			frames++
			mu.Unlock()
			if connection.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "native response"}) != nil {
				return
			}
			if connection.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "response", "output": []any{}}}) != nil {
				return
			}
		}
	}))
	t.Cleanup(host.Close)
	f := newCoordinatorAuthenticationFixture(t, "codex", strings.Replace(host.URL, "http://", "ws://", 1)+"/responses", false)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	old := f.coordinator.currentAgent.Runtime()
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("native request")}, Headers: sessionHeaders("native-retained", "conversation")}
	_, err := old.LargeModel.Model.Generate(ctx, call)
	require.NoError(t, err)
	result, err := f.store.SwitchAuthenticationAccount(ctx, config.ScopeWorkspace, f.capture(t), f.owner, f.second.ID)
	require.NoError(t, err)
	require.True(t, result.RuntimePublished)
	captured, ok, err := f.coordinator.currentAgent.Runtime().Snapshot.CapturedConstructionAccount(f.owner)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, f.second.ID, captured.ID)
	require.Equal(t, f.second.AccessToken, captured.AccessToken)
	require.Equal(t, f.second.RefreshToken, captured.RefreshToken)
	require.Equal(t, f.second.ExpiresAt, captured.ExpiresAt)
	require.JSONEq(t, string(f.second.Raw), string(captured.Raw))
	fresh := f.coordinator.currentAgent.Runtime()
	// A newer account-file selection must not replace the account metadata in
	// the accepted runtime. Only a later authorized mutation can publish it.
	require.NoError(t, accounts.Save(ctx, f.owner.AccountNamespace, f.first))
	_, err = fresh.LargeModel.Model.Generate(ctx, call)
	require.NoError(t, err)
	gotCredentials, gotAccounts, gotFrames := observed()
	require.Equal(t, []string{"Bearer " + f.first.AccessToken, "Bearer " + f.second.AccessToken}, gotCredentials)
	require.Equal(t, []string{"account-metadata-a", "account-metadata-b"}, gotAccounts)
	require.Equal(t, 2, gotFrames)
	_, err = f.store.LogoutAuthentication(ctx, config.ScopeWorkspace, f.capture(t), f.owner)
	require.NoError(t, err, "account/config mismatch must not veto explicit cleanup")
	for _, retained := range []InstalledRuntime{old, fresh, f.coordinator.currentAgent.Runtime()} {
		_, err = retained.LargeModel.Model.Generate(ctx, call)
		require.ErrorIs(t, err, config.ErrAuthenticationRevoked)
	}
	gotCredentials, gotAccounts, gotFrames = observed()
	require.Len(t, gotCredentials, 2, "logout must prevent native WebSocket handshakes")
	require.Equal(t, []string{"account-metadata-a", "account-metadata-b"}, gotAccounts)
	require.Equal(t, 2, gotFrames, "logout must prevent messages on retained native WebSocket connections")
	require.NoFileExists(t, f.marker)
}

func TestCoordinatorAuthenticationMutationDisabledTargetStaysDisabled(t *testing.T) {
	f := newCoordinatorAuthenticationFixture(t, "copilot", "https://example.invalid/v1", true)
	selected := f.store.RuntimeSnapshot().AgentModelState()
	_, err := f.store.SwitchAuthenticationAccount(t.Context(), config.ScopeWorkspace, f.capture(t), f.owner, f.second.ID)
	require.NoError(t, err)
	provider, ok := f.store.Config().Providers.Get(f.owner.ProviderID)
	require.True(t, ok)
	require.True(t, provider.Disable)
	require.Equal(t, f.second.AccessToken, provider.APIKey)
	require.Equal(t, selected, f.store.RuntimeSnapshot().AgentModelState())
	_, err = f.store.LogoutAuthentication(t.Context(), config.ScopeWorkspace, f.capture(t), f.owner)
	require.NoError(t, err)
	provider, ok = f.store.Config().Providers.Get(f.owner.ProviderID)
	require.True(t, ok)
	require.True(t, provider.Disable)
	require.Empty(t, provider.APIKey)
	require.Nil(t, provider.OAuthToken)
	require.Equal(t, selected, f.store.RuntimeSnapshot().AgentModelState())
	require.NoFileExists(t, f.marker)
}
