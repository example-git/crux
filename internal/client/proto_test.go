package client

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// peerChannelTestServer starts an HTTP server bound to a local unix socket
// that upgrades /v1/peer-channel to the peer-channel v2 subprotocol, so tests
// can exercise the real transport without insecure TCP.
func peerChannelTestServer(t *testing.T, handler func(*websocket.Conn, string)) *Client {
	t.Helper()
	root, err := os.MkdirTemp("", "crx-peer-channel-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	address := filepath.Join(root, "channel.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", address)
	require.NoError(t, err)

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/peer-channel", r.URL.Path)
		connection, err := (&websocket.Upgrader{Subprotocols: []string{proto.PeerChannelProtocol}}).Upgrade(w, r, nil)
		require.NoError(t, err)
		defer connection.Close()

		_, data, err := connection.ReadMessage()
		require.NoError(t, err)
		hello, err := proto.DecodePeerMessage(data, proto.PeerDirectionClientToServer)
		require.NoError(t, err)
		require.Equal(t, proto.PeerTypeHello, hello.Envelope.Type)
		epoch := hello.Envelope.Epoch

		data, err = proto.EncodePeerMessage(epoch, 1, "server-1", hello.Envelope.MessageID, "", proto.PeerTypeAcknowledgement, proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK})
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))
		data, err = proto.EncodePeerMessage(epoch, 2, "server-2", "", "", proto.PeerTypeReady, proto.PeerReady{})
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))

		_, data, err = connection.ReadMessage()
		require.NoError(t, err)
		attach, err := proto.DecodePeerMessage(data, proto.PeerDirectionClientToServer)
		require.NoError(t, err)
		require.Equal(t, proto.PeerTypeWorkspaceAttach, attach.Envelope.Type)

		data, err = proto.EncodePeerMessage(epoch, 3, "server-3", attach.Envelope.MessageID, attach.Envelope.WorkspaceID, proto.PeerTypeAcknowledgement, proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK})
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))

		handler(connection, epoch)
	})}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("peer channel fixture did not stop")
		}
	})

	client, err := NewClient(t.TempDir(), "unix", address)
	require.NoError(t, err)
	return client
}

func TestBrowseAndCloseIdleWorkspace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/browser":
			require.Equal(t, "/srv/project", request.URL.Query().Get("path"))
			jsonEncodeTest(t, writer, proto.BrowserListing{Path: "/srv/project"})
		case request.Method == http.MethodDelete && request.URL.Path == "/v1/workspaces/workspace/idle":
			writer.WriteHeader(http.StatusOK)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := captureClient(t, server)
	listing, err := client.Browse(t.Context(), "/srv/project")
	require.NoError(t, err)
	require.Equal(t, "/srv/project", listing.Path)
	require.NoError(t, client.CloseIdleWorkspace(t.Context(), "workspace"))
}

func jsonEncodeTest(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(writer).Encode(value))
}

func TestWorkspaceResponsesBindExactProviderOwners(t *testing.T) {
	owner := providerregistry.RegistrationOwner{
		ProviderID:           "same",
		AccountNamespace:     "same-account",
		Construction:         providerregistry.ConstructionGenericJSON,
		CompatibilityAdapter: providerregistry.ConstructionOpenAICompat,
		HasManifest:          true,
		ManifestID:           "plugin.same",
		ManifestVersion:      "1.2.3",
	}
	workspace := proto.Workspace{
		ID:     "workspace",
		Config: &config.Config{},
		ProviderSurfaces: []providerregistry.Surface{
			{ID: owner.ProviderID, Owner: &owner},
			{ID: "unavailable"},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/workspaces":
			jsonEncodeTest(t, writer, []proto.Workspace{workspace})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/workspaces/workspace":
			jsonEncodeTest(t, writer, workspace)
		case request.Method == http.MethodPost && request.URL.Path == "/v1/workspaces":
			jsonEncodeTest(t, writer, workspace)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := captureClient(t, server)

	listed, err := client.ListWorkspaces(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	created, err := client.CreateWorkspace(t.Context(), proto.Workspace{})
	require.NoError(t, err)
	got, err := client.GetWorkspace(t.Context(), workspace.ID)
	require.NoError(t, err)
	for _, cfg := range []*config.Config{listed[0].Config, created.Config, got.Config} {
		bound, ok := cfg.ProviderOwner(owner.ProviderID)
		require.True(t, ok)
		require.Equal(t, owner, bound)
		_, ok = cfg.ProviderOwner("unavailable")
		require.False(t, ok)
	}
}

func TestWorkspaceResponsesRejectMismatchedProviderOwners(t *testing.T) {
	workspace := proto.Workspace{
		ID:     "workspace",
		Config: &config.Config{},
		ProviderSurfaces: []providerregistry.Surface{{
			ID:    "same",
			Owner: &providerregistry.RegistrationOwner{ProviderID: "other"},
		}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		jsonEncodeTest(t, writer, workspace)
	}))
	defer server.Close()

	_, err := captureClient(t, server).GetWorkspace(t.Context(), workspace.ID)
	require.ErrorContains(t, err, "mismatched owner")
}

func TestCreateWorkspaceRejectsLegacyForwardingWithoutTransmission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("legacy private state must not be transmitted")
	}))
	defer server.Close()
	client := captureClient(t, server)
	created, err := client.CreateWorkspace(t.Context(), proto.Workspace{
		ForwardedProviders: map[string]config.ProviderConfig{"remote": {ID: "remote", APIKey: "secret"}},
		ForwardedAccounts:  map[string]config.ForwardedAccount{"remote": {Entry: accounts.Entry{ID: "account", AccessToken: "token"}}},
	})
	require.ErrorContains(t, err, "legacy forwarding is unsupported")
	require.Nil(t, created)
}

func TestCreateWorkspaceRejectsUnknownAuthorityModeWithoutTransmission(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Error("invalid authority mode must not be transmitted")
	}))
	defer server.Close()
	created, err := captureClient(t, server).CreateWorkspace(t.Context(), proto.Workspace{AuthorityMode: "unknown"})
	require.ErrorContains(t, err, "unsupported workspace authority mode")
	require.Nil(t, created)
}

func TestSubscribeEventsContextCancelClosesEvents(t *testing.T) {
	t.Parallel()

	event := proto.PeerResourceEvent[proto.AgentEvent]{Type: string(pubsub.CreatedEvent), Payload: proto.AgentEvent{Type: proto.AgentEventTypeResponse}}
	firstEventSent := make(chan struct{})
	writeSecondEvent := make(chan struct{})

	c := peerChannelTestServer(t, func(connection *websocket.Conn, epoch string) {
		data, err := proto.EncodePeerMessage(epoch, 4, "server-4", "", "ws1", proto.PeerTypeEventAgent, event)
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))
		close(firstEventSent)
		select {
		case <-writeSecondEvent:
		case <-time.After(5 * time.Second):
			return
		}
		data, err = proto.EncodePeerMessage(epoch, 5, "server-5", "", "ws1", proto.PeerTypeEventAgent, event)
		require.NoError(t, err)
		_ = connection.WriteMessage(websocket.TextMessage, data)
		_, _, _ = connection.ReadMessage()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := c.SubscribeEvents(ctx, "ws1")
	require.NoError(t, err)

	select {
	case <-firstEventSent:
	case <-time.After(5 * time.Second):
		require.Fail(t, "timed out waiting for server event")
	}

	select {
	case <-events:
	case <-time.After(5 * time.Second):
		require.Fail(t, "timed out waiting for first event")
	}

	cancel()
	close(writeSecondEvent)

	// Unsubscription runs asynchronously relative to the context
	// cancellation, so a message already in flight from the server (the
	// second event, released above) may still land on the channel before
	// the subscriber is removed. Drain any such in-flight events and only
	// require that the channel eventually closes.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			require.Fail(t, "timed out waiting for event channel close")
			return
		}
	}
}

func TestSubscribeEventsPreservesOpaqueMessageMetadata(t *testing.T) {
	t.Parallel()

	payloadBytes := []byte(`{ "number" : 1.00e+2, "ordered" : [2,1] }`)
	envelope, err := message.NewProviderMetadataEnvelope("missing.plugin", 17, message.ProviderMetadataScopeContinuation, payloadBytes)
	require.NoError(t, err)
	event := proto.PeerResourceEvent[proto.Message]{Type: string(pubsub.UpdatedEvent), Payload: proto.Message{Role: proto.Assistant, Parts: []proto.ContentPart{
		proto.ProviderMetadataContent{ProviderMetadata: message.ProviderMetadata{envelope}},
	}}}

	c := peerChannelTestServer(t, func(connection *websocket.Conn, epoch string) {
		data, err := proto.EncodePeerMessage(epoch, 4, "server-4", "", "ws1", proto.PeerTypeEventMessage, event)
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))
		_, _, _ = connection.ReadMessage()
	})

	events, err := c.SubscribeEvents(t.Context(), "ws1")
	require.NoError(t, err)
	event2, ok := <-events
	require.True(t, ok)
	decoded, ok := event2.(pubsub.Event[proto.Message])
	require.True(t, ok, "unexpected channel event %T", event2)
	metadata := decoded.Payload.Parts[0].(proto.ProviderMetadataContent).ProviderMetadata
	require.Len(t, metadata, 1)
	require.Equal(t, message.ProviderMetadataScopeContinuation, metadata[0].Scope)
	require.Equal(t, payloadBytes, metadata[0].Payload)
}

func TestCreateAgentDefinitionSendsConfigurationAndReturnsPath(t *testing.T) {
	t.Parallel()

	defaultFormat := "json"
	request := proto.CreateAgentDefinitionRequest{
		Scope:       "project",
		Name:        "reviewer",
		Description: "Reviews changes",
		Model:       "provider/model",
		Tools:       []string{"script"},
		Script: &proto.AgentDefinitionScript{
			Path:    "./scripts/review.py",
			Timeout: "30s",
			Variables: map[string]proto.AgentDefinitionScriptVariable{
				"input":  {Required: true},
				"format": {Default: &defaultFormat, Values: []string{"json", "text"}},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/workspaces/ws1/agent/definitions", r.URL.Path)
		var received proto.CreateAgentDefinitionRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		require.Equal(t, request, received)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		require.NoError(t, json.NewEncoder(w).Encode(proto.CreateAgentDefinitionResponse{Path: "/project/.ai-cli/agents/reviewer.md"}))
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	path, err := c.CreateAgentDefinition(context.Background(), "ws1", request)
	require.NoError(t, err)
	require.Equal(t, "/project/.ai-cli/agents/reviewer.md", path)
}

func TestGetAgentInstructionsReturnsTypedSnapshot(t *testing.T) {
	t.Parallel()

	want := agent.InstructionSnapshot{
		ProviderID: "anthropic",
		ModelID:    "claude-test",
		Policy:     fantasy.InstructionPolicyAnthropic,
		Sections: []agent.InstructionSnapshotSection{
			{
				Kind:          fantasy.InstructionKindTooling,
				Stability:     fantasy.InstructionStabilityStatic,
				Text:          "tooling",
				CacheBoundary: true,
			},
			{
				Kind:      fantasy.InstructionKindProviderContext,
				Stability: fantasy.InstructionStabilityDynamic,
				Text:      "provider context",
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/workspaces/ws1/agent/instructions", r.URL.Path)
		jsonEncodeTest(t, w, want)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	got, err := c.GetAgentInstructions(t.Context(), "ws1")
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestCreateAgentDefinitionReturnsServerValidationError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(proto.Error{Message: "invalid agent definition: invalid scope"})
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	_, err := c.CreateAgentDefinition(context.Background(), "ws1", proto.CreateAgentDefinitionRequest{})
	require.ErrorContains(t, err, "invalid scope")
}

func TestSendMessageAcceptsStatusAccepted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	require.NoError(t, c.SendMessage(context.Background(), "ws1", "sess1", "", "hello"))
}

func TestSendMessagePropagatesSubmissionID(t *testing.T) {
	t.Parallel()

	var received proto.AgentMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	for _, mode := range []agent.DeliveryMode{agent.DeliveryQueue, agent.DeliverySteer} {
		ctx := agent.WithSubmissionID(context.Background(), "submission-id")
		ctx = agent.WithDeliveryMode(ctx, mode)
		require.NoError(t, c.SendMessageWithPermissionMode(ctx, "ws1", "sess1", "", "hello", proto.AgentPermissionDeny))
		require.Equal(t, "submission-id", received.SubmissionID)
		require.Equal(t, string(mode), received.DeliveryMode)
		require.Equal(t, proto.AgentPermissionDeny, received.PermissionMode)
	}
}

func TestSendMessagePropagatesPermissionMode(t *testing.T) {
	t.Parallel()

	var received proto.AgentMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	require.NoError(t, c.SendMessageWithPermissionMode(context.Background(), "ws1", "sess1", "", "hello", proto.AgentPermissionDeny))
	require.Equal(t, proto.AgentPermissionDeny, received.PermissionMode)
}

func TestForkSessionPreservesEstimatedUsageMetadata(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/workspaces/ws1/sessions/source/fork", r.URL.Path)
		_ = json.NewEncoder(w).Encode(proto.Session{ID: "forked", EstimatedUsage: true})
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	forked, err := c.ForkSession(context.Background(), "ws1", "source")
	require.NoError(t, err)
	require.Equal(t, "forked", forked.ID)
	require.True(t, forked.EstimatedUsage)
}

func TestSendMessageAcceptsStatusOK(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	require.NoError(t, c.SendMessage(context.Background(), "ws1", "sess1", "", "hello"))
}

func TestSendMessageDecodesErrorBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(proto.Error{Message: "session id is required"})
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	err := c.SendMessage(context.Background(), "ws1", "", "", "hello")
	require.Error(t, err)
	require.Contains(t, err.Error(), "status code 400")
	require.Contains(t, err.Error(), "session id is required")
}

func TestSendMessageFallsBackOnMalformedErrorBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	err := c.SendMessage(context.Background(), "ws1", "sess1", "", "hello")
	require.Error(t, err)
	require.Contains(t, err.Error(), "status code 500")
	require.NotContains(t, err.Error(), "not json")
}

func TestSendMessageFallsBackOnEmptyErrorBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	err := c.SendMessage(context.Background(), "ws1", "sess1", "", "hello")
	require.Error(t, err)
	require.Contains(t, err.Error(), "status code 500")
}
