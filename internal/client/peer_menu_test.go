package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/proto"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// newPeerMenuTestServer starts a TLS test server that negotiates runtime
// capabilities over HTTP and then serves a single connection-scoped v2 peer
// channel: hello/ready handshake (no workspace attach, matching the menu
// commands' PeerScopeConnection contract), followed by exactly one client
// command decoded and handed to handle. handle may push additional
// server-to-client messages (e.g. progress events) using the provided
// connection/epoch/sequence before returning the command's acknowledgement.
func newPeerMenuTestServer(t *testing.T, handle func(conn *websocket.Conn, epoch string, sequence *uint64, command proto.PeerDecodedMessage) proto.PeerAcknowledgement) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{
		Subprotocols: []string{proto.PeerChannelProtocol},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runtime-capabilities" {
			require.NoError(t, json.NewEncoder(w).Encode(workspaceChannelTestCapabilities("")))
			return
		}
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/peer-channel", r.URL.Path)
		connection, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer connection.Close()

		_, data, err := connection.ReadMessage()
		require.NoError(t, err)
		hello, err := proto.DecodePeerMessage(data, proto.PeerDirectionClientToServer)
		require.NoError(t, err)
		require.Equal(t, proto.PeerTypeHello, hello.Envelope.Type)
		epoch := hello.Envelope.Epoch
		var sequence uint64 = 1

		data, err = proto.EncodePeerMessage(epoch, sequence, "server-hello-ack", hello.Envelope.MessageID, "", proto.PeerTypeAcknowledgement, proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK})
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))
		sequence++

		data, err = proto.EncodePeerMessage(epoch, sequence, "server-ready", "", "", proto.PeerTypeReady, proto.PeerReady{})
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))
		sequence++

		_, data, err = connection.ReadMessage()
		require.NoError(t, err)
		command, err := proto.DecodePeerMessage(data, proto.PeerDirectionClientToServer)
		require.NoError(t, err)
		acknowledgement := handle(connection, epoch, &sequence, command)
		data, err = proto.EncodePeerMessage(epoch, sequence, "server-command-ack", command.Envelope.MessageID, command.Envelope.WorkspaceID, proto.PeerTypeAcknowledgement, acknowledgement)
		require.NoError(t, err)
		require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))

		// Keep the connection open briefly so the client can finish
		// reading before the handler returns and closes it.
		time.Sleep(200 * time.Millisecond)
	}))
}

func newPeerMenuTestClient(server *httptest.Server) *Client {
	return &Client{h: server.Client(), network: "tcp", addr: strings.TrimPrefix(server.URL, "https://"), secure: true, clientID: "test"}
}

func TestRefreshWorkspacesViaPeer(t *testing.T) {
	want := []proto.Workspace{{ID: "workspace-1", Path: "/workspace-1"}}
	server := newPeerMenuTestServer(t, func(_ *websocket.Conn, _ string, _ *uint64, command proto.PeerDecodedMessage) proto.PeerAcknowledgement {
		require.Equal(t, proto.PeerTypeWorkspaceList, command.Envelope.Type)
		require.Empty(t, command.Envelope.WorkspaceID)
		data, err := json.Marshal(want)
		require.NoError(t, err)
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Data: data}
	})
	defer server.Close()
	c := newPeerMenuTestClient(server)
	workspaces, err := c.RefreshWorkspacesViaPeer(t.Context())
	require.NoError(t, err)
	require.Len(t, workspaces, 1)
	require.Equal(t, "workspace-1", workspaces[0].ID)
}

func TestBrowseViaPeer(t *testing.T) {
	want := proto.BrowserListing{Path: "/root", Entries: []proto.BrowserEntry{{Name: "child", Directory: true}}}
	server := newPeerMenuTestServer(t, func(_ *websocket.Conn, _ string, _ *uint64, command proto.PeerDecodedMessage) proto.PeerAcknowledgement {
		require.Equal(t, proto.PeerTypeBrowserList, command.Envelope.Type)
		require.Equal(t, "/root", command.Payload.(*proto.PeerBrowserListRequest).Path)
		data, err := json.Marshal(want)
		require.NoError(t, err)
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Data: data}
	})
	defer server.Close()
	c := newPeerMenuTestClient(server)
	listing, err := c.BrowseViaPeer(t.Context(), "/root")
	require.NoError(t, err)
	require.Equal(t, want, listing)
}

// TestCreateProjectDeliversProgressAndResult verifies that progress lines
// pushed before the command's own acknowledgement are delivered to the
// caller's progress callback, correlated by the CreateProject-generated
// message ID, and that the final result still decodes correctly.
func TestCreateProjectDeliversProgressAndResult(t *testing.T) {
	server := newPeerMenuTestServer(t, func(conn *websocket.Conn, epoch string, sequence *uint64, command proto.PeerDecodedMessage) proto.PeerAcknowledgement {
		require.Equal(t, proto.PeerTypeWorkspaceCreate, command.Envelope.Type)
		request := command.Payload.(*proto.PeerWorkspaceCreateRequest)
		require.Equal(t, proto.PeerWorkspaceCreateGitInit, request.Mode)

		data, err := proto.EncodePeerMessage(epoch, *sequence, "server-progress", "", "", proto.PeerTypeWorkspaceCreateProgress, proto.PeerWorkspaceCreateProgress{RequestID: command.Envelope.MessageID, Line: "Initialized empty Git repository"})
		require.NoError(t, err)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))
		*sequence++

		resultData, err := json.Marshal(proto.PeerWorkspaceCreateResult{Path: "/root/new-project"})
		require.NoError(t, err)
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Data: resultData}
	})
	defer server.Close()
	c := newPeerMenuTestClient(server)

	var mu sync.Mutex
	var lines []string
	path, err := c.CreateProject(t.Context(), proto.PeerWorkspaceCreateRequest{Root: "/root", RelativePath: "new-project", Mode: proto.PeerWorkspaceCreateGitInit}, func(line string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, line)
	})
	require.NoError(t, err)
	require.Equal(t, "/root/new-project", path)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"Initialized empty Git repository"}, lines)
}

func TestSubscribeWorkspaceListChangedReceivesPush(t *testing.T) {
	var pushed atomic.Bool
	server := newPeerMenuTestServer(t, func(conn *websocket.Conn, epoch string, sequence *uint64, command proto.PeerDecodedMessage) proto.PeerAcknowledgement {
		require.Equal(t, proto.PeerTypeWorkspaceList, command.Envelope.Type)
		data, err := proto.EncodePeerMessage(epoch, *sequence, "server-push", "", "", proto.PeerTypeWorkspaceListChanged, proto.PeerWorkspaceListChanged{})
		require.NoError(t, err)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))
		*sequence++
		pushed.Store(true)
		return proto.PeerAcknowledgement{Status: proto.WorkspaceChannelStatusOK, Data: json.RawMessage(`[]`)}
	})
	defer server.Close()
	c := newPeerMenuTestClient(server)

	sub, unsubscribe, err := c.SubscribeWorkspaceListChanged(t.Context())
	require.NoError(t, err)
	defer unsubscribe()

	// Issue a command over the same connection so the server-side handler
	// proceeds far enough to push the WorkspaceListChanged event.
	_, err = c.RefreshWorkspacesViaPeer(t.Context())
	require.NoError(t, err)

	select {
	case <-sub:
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive workspace list changed push")
	}
	require.True(t, pushed.Load())
}
