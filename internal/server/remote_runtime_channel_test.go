package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/proto"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func dialRemoteRuntimeChannel(t *testing.T, serverURL string, httpClient *http.Client, workspaceID, clientID string, authority *config.RemoteAuthority) *websocket.Conn {
	t.Helper()
	connection, response, err := openRemoteRuntimeChannel(t, serverURL, httpClient, workspaceID, clientID, authority)
	if response != nil {
		defer response.Body.Close()
	}
	if err != nil {
		body := ""
		if response != nil {
			data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			body = string(data)
		}
		require.NoErrorf(t, err, "workspace channel upgrade status=%v body=%q", responseStatus(response), body)
	}
	require.Equal(t, proto.WorkspaceChannelProtocol, connection.Subprotocol())
	return connection
}

func openRemoteRuntimeChannel(t *testing.T, serverURL string, httpClient *http.Client, workspaceID, clientID string, authority *config.RemoteAuthority) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	endpoint := "wss" + strings.TrimPrefix(serverURL, "https") + "/v1/workspaces/" + url.PathEscape(workspaceID) + "/channel?client_id=" + url.QueryEscape(clientID)
	headers := http.Header{
		"Crux-Runtime-Protocol":      {proto.RemoteRuntimeProtocol},
		cruxlog.EphemeralStateHeader: {"1"},
	}
	if authority != nil {
		(proto.WorkspaceAttachment{Mode: authority.Mode, Revision: authority.Revision, Digest: authority.Digest}).SetHeaders(headers)
	}
	transport, ok := httpClient.Transport.(*http.Transport)
	require.True(t, ok)
	dialer := websocket.Dialer{TLSClientConfig: transport.TLSClientConfig.Clone(), Subprotocols: []string{proto.WorkspaceChannelProtocol}}
	return dialer.DialContext(t.Context(), endpoint, headers)
}

func responseStatus(response *http.Response) int {
	if response == nil {
		return 0
	}
	return response.StatusCode
}

func sendRemoteRuntimeCommand(t *testing.T, connection *websocket.Conn, frame proto.WorkspaceChannelFrame) proto.WorkspaceChannelAcknowledgement {
	t.Helper()
	data, err := json.Marshal(frame)
	require.NoError(t, err)
	writeRemoteRuntimeFrame(t, connection, data)
	return readRemoteRuntimeAcknowledgement(t, connection, frame.CommandID)
}

func writeRemoteRuntimeFrame(t *testing.T, connection *websocket.Conn, data []byte) {
	t.Helper()
	require.NoError(t, connection.SetWriteDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, connection.WriteMessage(websocket.TextMessage, data))
}

func readRemoteRuntimeAcknowledgement(t *testing.T, connection *websocket.Conn, commandID string) proto.WorkspaceChannelAcknowledgement {
	t.Helper()
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
	for {
		messageType, data, err := connection.ReadMessage()
		require.NoError(t, err)
		require.Equal(t, websocket.TextMessage, messageType)
		frame, err := proto.DecodeWorkspaceChannelFrame(data)
		require.NoError(t, err)
		if frame.Type != proto.WorkspaceChannelAcknowledgementFrame || frame.CommandID != commandID {
			continue
		}
		return *frame.Acknowledgement
	}
}
