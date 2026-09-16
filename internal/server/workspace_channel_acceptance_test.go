package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func createRefreshChannelWorkspace(t *testing.T) (*httptest.Server, map[string]*http.Client, *Server, proto.Workspace, string, providerregistry.RegistrationOwner) {
	t.Helper()
	hs, clients, srv := newRemoteAuthorityTLSHarnessWithServer(t)
	workspacePath := filepath.Join(filepath.Dir(os.Getenv("HOME")), "channel-workspace")
	require.NoError(t, os.MkdirAll(workspacePath, 0o700))
	workspacePath, err := filepath.EvalSymlinks(workspacePath)
	require.NoError(t, err)
	models := []catalog.Model{{ID: "fixture", Name: "Fixture", ContextWindow: 8192, DefaultMaxTokens: 1024}}
	registration, bundle, err := registrytest.BundleFor("codex", "wss://fixture.invalid/responses", models)
	require.NoError(t, err)
	require.NotNil(t, registration.OAuth)
	owner := registration.Owner()
	provider := config.ProviderConfig{
		ID: "codex", Name: "Fixture", Type: catalog.TypeOpenAICompat, BaseURL: "wss://fixture.invalid/responses", Models: models,
		Plugin: &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version},
		Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction, CompatibilityAdapter: registration.CompatibilityAdapter},
	}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1, Bundles: []providerplugin.TransportBundle{bundle},
		Providers: []config.RemoteProviderDefinition{{NativeIdentity: &config.NativeIdentity{UserAgent: "fixture-client/1.2.3 (FixtureOS 1; fixture) FixtureTerminal", Version: "1.2.3", Originator: "fixture-client"}, Config: provider, BundleDigest: bundle.Digest}},
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: provider.ID, Model: models[0].ID},
			config.SelectedModelTypeSmall: {Provider: provider.ID, Model: models[0].ID},
		},
		Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, Account: &accounts.Entry{ID: "selected", AccessToken: "synthetic-old", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}}},
	}
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	clientID := uuid.NewString()
	body, err := json.Marshal(proto.CreateWorkspaceRequest{Workspace: proto.Workspace{Path: workspacePath, ClientID: clientID}, AuthorityMode: "client", Runtime: &proposal})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, hs.URL+"/v1/workspaces", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Crux-Runtime-Protocol", proto.RemoteRuntimeProtocol)
	request.Header.Set(cruxlog.EphemeralStateHeader, "1")
	response, err := clients["retained"].Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(data))
	var workspace proto.Workspace
	require.NoError(t, json.Unmarshal(data, &workspace))
	require.NotNil(t, workspace.Authority)
	return hs, clients, srv, workspace, clientID, owner
}

func TestWorkspaceChannelAttachmentReplayAndDetach(t *testing.T) {
	hs, clients, srv, workspace, clientID, owner := createRefreshChannelWorkspace(t)
	receiver, err := srv.backend.GetWorkspace(workspace.ID)
	require.NoError(t, err)

	mismatch := *workspace.Authority
	mismatch.Digest = string(bytes.Repeat([]byte{'0'}, 64))
	connection, response, err := openRemoteRuntimeChannel(t, hs.URL, clients["retained"], workspace.ID, clientID, &mismatch)
	require.Error(t, err)
	require.Nil(t, connection)
	require.NotNil(t, response)
	require.Equal(t, http.StatusConflict, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.Equal(t, 0, backend.WorkspaceLiveStreamCountForTest(receiver))

	refreshCtx, cancelRefresh := context.WithCancel(t.Context())
	refreshResult := make(chan error, 1)
	go func() {
		_, err := receiver.Cfg.RequestClientRefresh(refreshCtx, receiver.Cfg.RuntimeSnapshot(), owner)
		refreshResult <- err
	}()
	require.Eventually(t, func() bool { return len(receiver.Cfg.PendingClientRefreshes()) == 1 }, time.Second, 10*time.Millisecond)
	pending := receiver.Cfg.PendingClientRefreshes()[0]

	channel := dialRemoteRuntimeChannel(t, hs.URL, clients["retained"], workspace.ID, clientID, workspace.Authority)
	require.Eventually(t, func() bool { return backend.WorkspaceLiveStreamCountForTest(receiver) == 1 }, time.Second, 10*time.Millisecond)
	require.NoError(t, channel.SetReadDeadline(time.Now().Add(5*time.Second)))
	messageType, data, err := channel.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, messageType)
	frame, err := proto.DecodeWorkspaceChannelFrame(data)
	require.NoError(t, err)
	require.Equal(t, proto.WorkspaceChannelEventFrame, frame.Type)
	var envelope pubsub.Payload
	require.NoError(t, json.Unmarshal(frame.Event, &envelope))
	require.Equal(t, pubsub.PayloadTypeClientRefresh, envelope.Type)
	var event pubsub.Event[config.ClientRefreshRequest]
	require.NoError(t, json.Unmarshal(envelope.Payload, &event))
	require.Equal(t, pending, event.Payload)

	require.NoError(t, channel.Close())
	require.Eventually(t, func() bool { return backend.WorkspaceLiveStreamCountForTest(receiver) == 0 }, time.Second, 10*time.Millisecond)
	cancelRefresh()
	require.ErrorIs(t, <-refreshResult, context.Canceled)
}

func TestWorkspaceChannelRejectsMalformedBinaryAndOversizedFrames(t *testing.T) {
	hs, clients, _, workspace, clientID, _ := createRefreshChannelWorkspace(t)

	t.Run("malformed correlated command", func(t *testing.T) {
		channel := dialRemoteRuntimeChannel(t, hs.URL, clients["retained"], workspace.ID, clientID, workspace.Authority)
		writeRemoteRuntimeFrame(t, channel, []byte(`{"type":"runtime.replace","command_id":"malformed","runtime_replace":{}}`))
		ack := readRemoteRuntimeAcknowledgement(t, channel, "malformed")
		require.Equal(t, proto.WorkspaceChannelStatusInvalid, ack.Status)
		_ = channel.Close()
	})

	t.Run("binary", func(t *testing.T) {
		channel := dialRemoteRuntimeChannel(t, hs.URL, clients["retained"], workspace.ID, clientID, workspace.Authority)
		require.NoError(t, channel.SetWriteDeadline(time.Now().Add(5*time.Second)))
		err := channel.WriteMessage(websocket.BinaryMessage, []byte(`{"type":"runtime.refresh_complete"}`))
		if err == nil {
			require.NoError(t, channel.SetReadDeadline(time.Now().Add(5*time.Second)))
			_, _, err = channel.ReadMessage()
		}
		require.Error(t, err)
		_ = channel.Close()
	})

	t.Run("oversized", func(t *testing.T) {
		channel := dialRemoteRuntimeChannel(t, hs.URL, clients["retained"], workspace.ID, clientID, workspace.Authority)
		require.NoError(t, channel.SetWriteDeadline(time.Now().Add(15*time.Second)))
		err := channel.WriteMessage(websocket.TextMessage, bytes.Repeat([]byte{'x'}, proto.MaxWorkspaceChannelFrameBytes+1))
		if err == nil {
			require.NoError(t, channel.SetReadDeadline(time.Now().Add(5*time.Second)))
			_, _, err = channel.ReadMessage()
		}
		require.Error(t, err)
		_ = channel.Close()
	})
}
