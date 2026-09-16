package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/proto"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// dialTestPeerChannel opens a v2 peer channel against a real, running
// *Server and completes the hello/ready handshake exactly as
// internal/client/workspace_channel.go's openPeerChannel does, so these
// tests exercise the genuine wire dispatch path (encode/decode, sequencing,
// journaling) rather than calling server methods directly.
func dialTestPeerChannel(t *testing.T, serverURL, clientID string) (*websocket.Conn, string) {
	t.Helper()
	endpoint := "ws" + strings.TrimPrefix(serverURL, "http") + "/v1/peer-channel?client_id=" + clientID
	headers := http.Header{
		"Crux-Runtime-Protocol":      {proto.RemoteRuntimeProtocol},
		cruxlog.EphemeralStateHeader: {"1"},
	}
	dialer := websocket.Dialer{Subprotocols: []string{proto.PeerChannelProtocol}}
	conn, response, err := dialer.Dial(endpoint, headers)
	if response != nil {
		defer response.Body.Close()
	}
	require.NoError(t, err)
	require.Equal(t, proto.PeerChannelProtocol, conn.Subprotocol())

	epoch := strings.ReplaceAll(uuid.NewString(), "-", "")
	data, err := proto.EncodePeerMessage(epoch, 1, uuid.NewString(), "", "", proto.PeerTypeHello, proto.PeerHello{ClientID: clientID})
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))

	_, data, err = conn.ReadMessage()
	require.NoError(t, err)
	ack, err := proto.DecodePeerMessage(data, proto.PeerDirectionServerToClient)
	require.NoError(t, err)
	require.Equal(t, proto.PeerTypeAcknowledgement, ack.Envelope.Type)
	require.Equal(t, proto.WorkspaceChannelStatusOK, ack.Payload.(*proto.PeerAcknowledgement).Status)

	_, data, err = conn.ReadMessage()
	require.NoError(t, err)
	ready, err := proto.DecodePeerMessage(data, proto.PeerDirectionServerToClient)
	require.NoError(t, err)
	require.Equal(t, proto.PeerTypeReady, ready.Envelope.Type)

	return conn, epoch
}

// sendTestPeerCommand sends a client-to-server command over an already
// handshaken peer channel and returns the matching acknowledgement,
// skipping over any interleaved server-pushed events (for example
// PeerTypeWorkspaceListChanged) so tests can assert on push delivery
// independently of command/response pairing.
func sendTestPeerCommand(t *testing.T, conn *websocket.Conn, epoch string, sequence *uint64, workspaceID string, messageType proto.PeerMessageType, payload any) proto.PeerAcknowledgement {
	t.Helper()
	messageID := uuid.NewString()
	*sequence++
	data, err := proto.EncodePeerMessage(epoch, *sequence, messageID, "", workspaceID, messageType, payload)
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))
	for {
		_, raw, err := conn.ReadMessage()
		require.NoError(t, err)
		decoded, err := proto.DecodePeerMessage(raw, proto.PeerDirectionServerToClient)
		require.NoError(t, err)
		if decoded.Envelope.Type != proto.PeerTypeAcknowledgement || decoded.Envelope.ReplyTo != messageID {
			continue
		}
		return *decoded.Payload.(*proto.PeerAcknowledgement)
	}
}

// readTestPeerEvent reads messages from conn until one of the given type is
// observed, returning its decoded message. It fails the test if the
// connection errors first.
func readTestPeerEvent(t *testing.T, conn *websocket.Conn, want proto.PeerMessageType) proto.PeerDecodedMessage {
	t.Helper()
	for {
		_, raw, err := conn.ReadMessage()
		require.NoError(t, err)
		decoded, err := proto.DecodePeerMessage(raw, proto.PeerDirectionServerToClient)
		require.NoError(t, err)
		if decoded.Envelope.Type == want {
			return decoded
		}
	}
}

func newTestPeerChannelServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	server := NewServer(nil, "unix", "test")
	t.Cleanup(server.backend.Shutdown)
	testServer := httptest.NewServer(server.h.Handler)
	t.Cleanup(testServer.Close)
	return server, testServer
}

func TestPeerChannelWorkspaceListRoundTrip(t *testing.T) {
	server, testServer := newTestPeerChannelServer(t)
	workspaceDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dataDir := t.TempDir()
	clientID := uuid.NewString()
	_, _, complete, err := server.backend.CreateWorkspaceForResponse(proto.Workspace{Path: workspaceDir, DataDir: dataDir, ClientID: clientID})
	require.NoError(t, err)
	defer complete()

	conn, epoch := dialTestPeerChannel(t, testServer.URL, clientID)
	defer conn.Close()
	var sequence uint64 = 1

	ack := sendTestPeerCommand(t, conn, epoch, &sequence, "", proto.PeerTypeWorkspaceList, proto.PeerWorkspaceListRequest{})
	require.Equal(t, proto.WorkspaceChannelStatusOK, ack.Status)
	var workspaces []proto.Workspace
	require.NoError(t, json.Unmarshal(ack.Data, &workspaces))
	require.Len(t, workspaces, 1)
	require.Equal(t, workspaceDir, workspaces[0].Path)
}

func TestPeerChannelBrowserListRoundTrip(t *testing.T) {
	server, testServer := newTestPeerChannelServer(t)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "child"), 0o700))
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.NoError(t, server.SetWorkspaceRoots([]string{canonicalRoot}))

	conn, epoch := dialTestPeerChannel(t, testServer.URL, uuid.NewString())
	defer conn.Close()
	var sequence uint64 = 1

	ack := sendTestPeerCommand(t, conn, epoch, &sequence, "", proto.PeerTypeBrowserList, proto.PeerBrowserListRequest{Path: root})
	require.Equal(t, proto.WorkspaceChannelStatusOK, ack.Status)
	var listing proto.BrowserListing
	require.NoError(t, json.Unmarshal(ack.Data, &listing))
	require.Equal(t, canonicalRoot, listing.Path)
	require.Len(t, listing.Entries, 1)
	require.Equal(t, "child", listing.Entries[0].Name)
}

func TestPeerChannelWorkspaceCreateRoundTrip(t *testing.T) {
	server, testServer := newTestPeerChannelServer(t)
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.NoError(t, server.SetWorkspaceRoots([]string{canonicalRoot}))

	conn, epoch := dialTestPeerChannel(t, testServer.URL, uuid.NewString())
	defer conn.Close()
	var sequence uint64 = 1

	ack := sendTestPeerCommand(t, conn, epoch, &sequence, "", proto.PeerTypeWorkspaceCreate, proto.PeerWorkspaceCreateRequest{
		Root: canonicalRoot, RelativePath: "new-project", Mode: proto.PeerWorkspaceCreatePlain,
	})
	require.Equal(t, proto.WorkspaceChannelStatusOK, ack.Status)
	var result proto.PeerWorkspaceCreateResult
	require.NoError(t, json.Unmarshal(ack.Data, &result))
	require.Equal(t, filepath.Join(canonicalRoot, "new-project"), result.Path)
	info, err := os.Stat(result.Path)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

// TestPeerChannelWorkspaceCreateGitInitStreamsProgress exercises the
// git-init mode end to end over the real wire protocol, including any
// PeerTypeWorkspaceCreateProgress telemetry events correlated by the create
// command's own message ID. Progress delivery is best-effort telemetry, so
// a line arriving is not asserted as a hard requirement, but any line that
// does arrive must carry the correct correlation ID.
func TestPeerChannelWorkspaceCreateGitInitStreamsProgress(t *testing.T) {
	requireGit(t)
	server, testServer := newTestPeerChannelServer(t)
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.NoError(t, server.SetWorkspaceRoots([]string{canonicalRoot}))

	conn, epoch := dialTestPeerChannel(t, testServer.URL, uuid.NewString())
	defer conn.Close()

	messageID := uuid.NewString()
	data, err := proto.EncodePeerMessage(epoch, 2, messageID, "", "", proto.PeerTypeWorkspaceCreate, proto.PeerWorkspaceCreateRequest{
		Root: canonicalRoot, RelativePath: "git-project", Mode: proto.PeerWorkspaceCreateGitInit,
	})
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))

	for {
		_, raw, err := conn.ReadMessage()
		require.NoError(t, err)
		decoded, err := proto.DecodePeerMessage(raw, proto.PeerDirectionServerToClient)
		require.NoError(t, err)
		if decoded.Envelope.Type == proto.PeerTypeWorkspaceCreateProgress {
			progress := decoded.Payload.(*proto.PeerWorkspaceCreateProgress)
			require.Equal(t, messageID, progress.RequestID)
			continue
		}
		if decoded.Envelope.Type == proto.PeerTypeAcknowledgement && decoded.Envelope.ReplyTo == messageID {
			ack := decoded.Payload.(*proto.PeerAcknowledgement)
			require.Equal(t, proto.WorkspaceChannelStatusOK, ack.Status)
			var result proto.PeerWorkspaceCreateResult
			require.NoError(t, json.Unmarshal(ack.Data, &result))
			_, statErr := os.Stat(filepath.Join(result.Path, ".git"))
			require.NoError(t, statErr)
			break
		}
	}
}

func TestPeerChannelMenuCommandsRequireAuthenticatedManagement(t *testing.T) {
	peer := &serverPeerChannel{controller: &controllerV1{}, authenticatedManagement: false}
	require.Equal(t, proto.WorkspaceChannelStatusForbidden, peer.listWorkspaces().Status)
	require.Equal(t, proto.WorkspaceChannelStatusForbidden, peer.browseList(proto.PeerBrowserListRequest{}).Status)
	require.Equal(t, proto.WorkspaceChannelStatusForbidden, peer.createWorkspaceDirectory("id", proto.PeerWorkspaceCreateRequest{}).Status)
}

// TestPeerChannelNotifiesWorkspaceListChangedOnCreate verifies that creating
// a workspace over plain HTTP pushes a connection-scoped
// PeerTypeWorkspaceListChanged event to every other open peer channel
// belonging to the same principal, without that connection polling.
func TestPeerChannelNotifiesWorkspaceListChangedOnCreate(t *testing.T) {
	_, testServer := newTestPeerChannelServer(t)
	watcherConn, watcherEpoch := dialTestPeerChannel(t, testServer.URL, uuid.NewString())
	defer watcherConn.Close()
	var watcherSequence uint64 = 1

	workspaceDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dataDir := t.TempDir()
	body := strings.NewReader(`{"path":"` + workspaceDir + `","data_dir":"` + dataDir + `","client_id":"` + uuid.NewString() + `"}`)
	response, err := http.Post(testServer.URL+"/v1/workspaces", "application/json", body)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)

	readTestPeerEvent(t, watcherConn, proto.PeerTypeWorkspaceListChanged)

	// The watcher can now re-list and observe the new workspace.
	ack := sendTestPeerCommand(t, watcherConn, watcherEpoch, &watcherSequence, "", proto.PeerTypeWorkspaceList, proto.PeerWorkspaceListRequest{})
	require.Equal(t, proto.WorkspaceChannelStatusOK, ack.Status)
	var workspaces []proto.Workspace
	require.NoError(t, json.Unmarshal(ack.Data, &workspaces))
	require.Len(t, workspaces, 1)
	require.Equal(t, workspaceDir, workspaces[0].Path)
}
