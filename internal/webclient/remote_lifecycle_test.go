package webclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/workspace"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestRemoteUsageReplayAndInvalidation(t *testing.T) {
	attachment := &remoteAttachment{listeners: make(map[chan []byte]struct{})}
	usage := func(revision uint64) pubsub.Event[proto.AgentEvent] {
		return pubsub.Event[proto.AgentEvent]{Type: pubsub.UpdatedEvent, Payload: proto.AgentEvent{
			Type: proto.AgentEventTypeUsage, ProviderID: "native",
			Usage: &config.ProviderUsageResult{Revision: revision, Digest: "runtime"},
		}}
	}
	attachment.publish(usage(1))
	attachment.publish(usage(2))
	host := httptest.NewServer(http.HandlerFunc(attachment.channel))
	defer host.Close()
	dialer := websocket.Dialer{Subprotocols: []string{proto.WorkspaceChannelProtocol}}
	socket, response, err := dialer.Dial("ws"+strings.TrimPrefix(host.URL, "http"), nil)
	if response != nil {
		defer response.Body.Close()
	}
	require.NoError(t, err)
	defer socket.Close()
	readUsage := func(revision uint64) {
		t.Helper()
		require.NoError(t, socket.SetReadDeadline(time.Now().Add(5*time.Second)))
		var frame proto.WorkspaceChannelFrame
		require.NoError(t, socket.ReadJSON(&frame))
		require.Equal(t, proto.WorkspaceChannelEventFrame, frame.Type)
		var payload pubsub.Payload
		require.NoError(t, json.Unmarshal(frame.Event, &payload))
		require.Equal(t, pubsub.PayloadTypeAgentEvent, payload.Type)
		var event pubsub.Event[proto.AgentEvent]
		require.NoError(t, json.Unmarshal(payload.Payload, &event))
		require.Equal(t, usage(revision), event)
	}
	readUsage(2)
	attachment.publish(usage(3))
	readUsage(3)
	attachment.publish(pubsub.Event[proto.ConfigChanged]{Type: pubsub.UpdatedEvent})
	attachment.mu.Lock()
	require.Empty(t, attachment.usageFrames)
	attachment.mu.Unlock()
	attachment.publish(usage(4))
	attachment.disconnect()
	attachment.mu.Lock()
	require.Empty(t, attachment.usageFrames)
	attachment.mu.Unlock()
}

func TestRemoteMessagePaginationHeaders(t *testing.T) {
	for _, more := range []string{"true", "false"} {
		t.Run(more, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/workspaces/remote/sessions/session/messages" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.URL.Query().Get("limit") != "120" || r.URL.Query().Get("before") != "anchor" {
					t.Errorf("pagination query was not preserved: %s", r.URL.RawQuery)
				}
				w.Header().Set("Crux-Messages-More", more)
				writeJSON(w, []any{})
			}))
			defer upstream.Close()
			native, err := client.NewClient(t.TempDir(), "tcp", strings.TrimPrefix(upstream.URL, "http://"))
			require.NoError(t, err)
			selected := workspace.NewProtocolClientWorkspace(native, proto.Workspace{ID: "remote", Config: &config.Config{}, Authority: &config.RemoteAuthority{Mode: "client", Revision: 1, Digest: "fixture"}})
			defer selected.Shutdown()
			defer native.CloseIdleConnections()
			owner := remoteOwner{connection: "fixture", clientID: "renderer"}
			manager := &remoteWorkspaces{owners: map[remoteOwner]*remoteSlot{owner: {attachment: &remoteAttachment{client: native, workspace: selected}}}}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/ui/connections/fixture/v1/workspaces/remote/sessions/session/messages?client_id=renderer&limit=120&before=anchor", nil)
			request.SetPathValue("name", "fixture")
			manager.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, more, response.Header().Get("Crux-Messages-More"))
			require.JSONEq(t, `[]`, response.Body.String())
		})
	}
}

func TestRemoteSessionSelectionRetriesRetainNativeIntent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "channel unavailable", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	native, err := client.NewClient(t.TempDir(), "tcp", strings.TrimPrefix(upstream.URL, "http://"))
	require.NoError(t, err)
	selected := workspace.NewProtocolClientWorkspace(native, proto.Workspace{ID: "remote", Config: &config.Config{}})
	defer selected.Shutdown()
	defer native.CloseIdleConnections()
	attachment := &remoteAttachment{client: native, workspace: selected}
	send := func(body string) int {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/current-session", strings.NewReader(body))
		attachment.selectSession(response, request)
		return response.Code
	}
	require.Equal(t, http.StatusBadGateway, send(`{"session_id":"session","selection_generation":1}`))
	intent := attachment.selectionIntent
	require.NotZero(t, intent.Generation)
	require.Equal(t, http.StatusBadGateway, send(`{"session_id":"session","selection_generation":1}`))
	require.Equal(t, intent, attachment.selectionIntent)
	require.Equal(t, http.StatusConflict, send(`{"session_id":"other","selection_generation":1}`))
	require.Equal(t, intent, attachment.selectionIntent)
}

func TestRemoteConfigViewUsesCollectedClientProvenance(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), []byte(`{"options":{"disable_default_providers":true},"providers":{"fixture":{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"synthetic-secret","models":[{"id":"main","default_max_tokens":100}]}},"models":{"large":{"provider":"fixture","model":"main"},"small":{"provider":"fixture","model":"main"}}}`), 0o600))
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"),
		"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"),
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfileCoreOnly),
	}))
	require.NoError(t, err)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config-view") {
			t.Error("client provenance requested from server")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	native, err := client.NewClient(root, "tcp", strings.TrimPrefix(upstream.URL, "http://"))
	require.NoError(t, err)
	native.SetLocalRuntimeStore(store)
	selected := workspace.NewProtocolClientWorkspace(native, proto.Workspace{
		ID: "remote", Path: root, Config: store.Config(), Runtime: &proposal, Creation: &proto.Workspace{Path: root},
		Authority: &config.RemoteAuthority{Mode: "client", Principal: "fixture", Revision: proposal.Revision, Digest: proposal.Digest},
	})
	defer selected.Shutdown()
	defer native.CloseIdleConnections()
	owner := remoteOwner{connection: "fixture", clientID: "renderer"}
	manager := &remoteWorkspaces{owners: map[remoteOwner]*remoteSlot{owner: {attachment: &remoteAttachment{client: native, workspace: selected}}}}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ui/connections/fixture/v1/workspaces/remote/config-view?client_id=renderer", nil)
	request.SetPathValue("name", "fixture")
	manager.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	var result proto.ConfigReadResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Equal(t, "remote", result.WorkspaceID)
	require.Equal(t, "main", result.View.Values["models.large.model"])
	require.Equal(t, filepath.Join(root, "crux.json"), result.View.Origins["models.large.model"].Path)
	require.NotContains(t, response.Body.String(), "synthetic-secret")
}

func TestRemoteSettingsRequireNativeClientOwner(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/workspaces/remote/codebase-index" {
			if r.Method != http.MethodGet {
				t.Errorf("unexpected upstream index mutation: %s", r.Method)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeJSON(w, proto.CodebaseIndexStatus{State: "ready"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	native, err := client.NewClient(t.TempDir(), "tcp", strings.TrimPrefix(upstream.URL, "http://"))
	require.NoError(t, err)
	selected := workspace.NewProtocolClientWorkspace(native, proto.Workspace{ID: "remote", Config: &config.Config{}, Authority: &config.RemoteAuthority{Mode: "client", Revision: 1, Digest: "fixture"}})
	defer selected.Shutdown()
	defer native.CloseIdleConnections()
	owner := remoteOwner{connection: "fixture", clientID: "renderer"}
	manager := &remoteWorkspaces{owners: map[remoteOwner]*remoteSlot{owner: {attachment: &remoteAttachment{client: native, workspace: selected}}}}
	send := func(method, body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/ui/connections/fixture/v1/workspaces/remote/codebase-index?client_id=renderer", strings.NewReader(body))
		request.SetPathValue("name", "fixture")
		manager.ServeHTTP(response, request)
		return response
	}
	response := send(http.MethodPost, `{"enabled":true}`)
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Contains(t, response.Body.String(), "owning client configuration is unavailable")
	require.Equal(t, http.StatusBadRequest, send(http.MethodPost, `{"enabled":true} {}`).Code)
	require.Equal(t, http.StatusMethodNotAllowed, send(http.MethodDelete, "").Code)
	response = send(http.MethodGet, "")
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"state":"ready"`)
	response = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ui/connections/fixture/v1/workspaces/remote/agent/default-small-model?client_id=renderer&provider_id=fixture", nil)
	request.SetPathValue("name", "fixture")
	manager.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Contains(t, response.Body.String(), "owning client catalog is unavailable")
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/ui/connections/fixture/v1/workspaces/remote/config-view?client_id=renderer", nil)
	request.SetPathValue("name", "fixture")
	manager.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "owning client configuration provenance is unavailable")
}

func TestRemoteAttachmentRetirementTracksRendererPresence(t *testing.T) {
	listener := make(chan []byte)
	attachment := &remoteAttachment{
		listeners:   map[chan []byte]struct{}{listener: {}},
		detachGrace: time.Hour,
		retire:      func(uint64) {},
	}
	attachment.mu.Lock()
	attachment.scheduleRetirementLocked()
	require.Nil(t, attachment.idleTimer)
	delete(attachment.listeners, listener)
	attachment.scheduleRetirementLocked()
	require.NotNil(t, attachment.idleTimer)
	first := attachment.idleTimer
	generation := attachment.idleGeneration
	attachment.scheduleRetirementLocked()
	require.Greater(t, attachment.idleGeneration, generation)
	require.False(t, first.Stop())
	attachment.listeners[listener] = struct{}{}
	attachment.closeLocked()
	require.True(t, attachment.closed)
	require.Empty(t, attachment.listeners)
	require.False(t, attachment.idleTimer.Stop())
	attachment.mu.Unlock()
	_, open := <-listener
	require.False(t, open)
}
