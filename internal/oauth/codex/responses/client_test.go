package responses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestDialAdvertisesRemoteCompactionV2(t *testing.T) {
	type requestData struct {
		path   string
		header http.Header
	}
	requests := make(chan requestData, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- requestData{path: r.URL.Path, header: r.Header.Clone()}
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		_ = conn.Close()
	}))
	defer server.Close()

	client := &client{
		url:        strings.Replace(server.URL, "http://", "ws://", 1) + "/responses",
		userAgent:  "crux-test",
		originator: "crux",
		version:    "test-version",
		headers: map[string]string{
			"x-codex-beta-features": "custom_feature,remote_compaction_v2",
		},
	}
	account := accountDiscriminator("acct_test", "test-token")
	compatibility := compatibilityIdentity(Name, account, "conversation-test", "conversation")
	conn, err := client.dial(context.Background(), "test-token", "acct_test", compatibility, "test-trace")
	require.NoError(t, err)
	_ = conn.Close()

	request := <-requests
	require.Equal(t, "/responses", request.path)
	require.Equal(t, "Bearer test-token", request.header.Get("Authorization"))
	require.Equal(t, "crux-test", request.header.Get("User-Agent"))
	require.Equal(t, "https://chatgpt.com", request.header.Get("Origin"))
	require.Equal(t, openaiBeta, request.header.Get("OpenAI-Beta"))
	require.Equal(t, compatibility, request.header.Get("session_id"))
	require.Equal(t, compatibility, request.header.Get("thread-id"))
	require.Equal(t, compatibility+":0", request.header.Get("x-codex-window-id"))
	require.Equal(t, compatibility, request.header.Get("x-client-request-id"))
	require.Equal(t, installationID, request.header.Get("x-codex-installation-id"))
	require.NotEqual(t, compatibility, installationID)
	require.Equal(t, "crux", request.header.Get("originator"))
	require.Equal(t, "test-version", request.header.Get("version"))
	require.Equal(t, "acct_test", request.header.Get("ChatGPT-Account-ID"))

	features := strings.Split(request.header.Get("x-codex-beta-features"), ",")
	require.Contains(t, features, "custom_feature")
	require.Equal(t, 1, strings.Count(request.header.Get("x-codex-beta-features"), remoteCompactionV2BetaFeature))

	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(request.header.Get("x-codex-turn-metadata")), &metadata))
	require.Equal(t, installationID, metadata["installation_id"])
	require.Equal(t, compatibility, metadata["session_id"])
	require.Equal(t, compatibility, metadata["thread_id"])
	require.Equal(t, compatibility+":0", metadata["window_id"])

	for _, secret := range []string{"conversation-test", "acct_test", "test-token"} {
		require.NotContains(t, compatibility, secret)
	}
}

func TestRequestClientMetadataIdentifiesCompaction(t *testing.T) {
	metadata := requestClientMetadata("compatibility", "compaction")
	require.Equal(t, installationID, metadata["x-codex-installation-id"])
	require.Equal(t, "compatibility", metadata["session_id"])
	require.Equal(t, "compatibility", metadata["thread_id"])
	require.Equal(t, "compatibility:0", metadata["x-codex-window-id"])

	var turnMetadata map[string]string
	require.NoError(t, json.Unmarshal([]byte(metadata["x-codex-turn-metadata"]), &turnMetadata))
	require.Equal(t, "compaction", turnMetadata["request_kind"])
	require.Equal(t, installationID, turnMetadata["installation_id"])
}
