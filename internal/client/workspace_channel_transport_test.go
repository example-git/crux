package client

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceChannelRejectsInsecureTCP(t *testing.T) {
	api, err := NewClient(t.TempDir(), "tcp", "127.0.0.1:1")
	require.NoError(t, err)
	_, err = api.SubscribeEvents(t.Context(), "ws1")
	require.ErrorContains(t, err, "refuses insecure TCP")
}

func testWorkspaceChannelTransport(t *testing.T, listener net.Listener, network, address string) {
	t.Helper()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/peer-channel", r.URL.Path)
		require.Equal(t, proto.RemoteRuntimeProtocol, r.Header.Get("Crux-Runtime-Protocol"))
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
		require.Equal(t, "ws1", attach.Envelope.WorkspaceID)

		data, err = proto.EncodePeerMessage(epoch, 3, "server-3", attach.Envelope.MessageID, "ws1", proto.PeerTypeAcknowledgement, proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK})
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))
		event := proto.PeerResourceEvent[proto.AgentEvent]{Type: string(pubsub.CreatedEvent), Payload: proto.AgentEvent{Type: proto.AgentEventTypeResponse}}
		data, err = proto.EncodePeerMessage(epoch, 4, "server-4", "", "ws1", proto.PeerTypeEventAgent, event)
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))
		_, _, _ = connection.ReadMessage()
	})}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("workspace channel fixture did not stop")
		}
	})

	api, err := NewClient(t.TempDir(), network, address)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	events, err := api.SubscribeEvents(ctx, "ws1")
	require.NoError(t, err)
	select {
	case event := <-events:
		_, ok := event.(pubsub.Event[proto.AgentEvent])
		require.True(t, ok, "unexpected channel event %T", event)
	case <-time.After(5 * time.Second):
		require.Fail(t, "timed out waiting for workspace channel event")
	}
	cancel()
}
