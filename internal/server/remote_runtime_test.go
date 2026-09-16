package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRemoteRuntimeTLSAdmissionAndOwnership(t *testing.T) {
	hs, clients := newRemoteAuthorityTLSHarness(t)
	// The normal shell description extracts the shared, non-secret search
	// executable on first use. Prepare it before measuring authority writes.
	_ = tools.NewBashTool(nil, nil, t.TempDir())
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CRUX_GLOBAL_CONFIG"), "crux.json"), []byte(`{"providers":{"client-only":{"id":"client-only","disable":true,"api_key":"synthetic-private-server-key","base_url":"https://server.invalid/v1","models":[{"id":"server-only-model"}]}},"models":{"large":{"provider":"client-only","model":"server-only-model"}}}`), 0o600))
	serverState := remoteServerState(t)
	defer func() {
		require.Equal(t, serverState, remoteServerState(t), "remote admission must preserve shared server state")
	}()
	path := filepath.Join(filepath.Dir(os.Getenv("HOME")), "workspace")
	require.NoError(t, os.MkdirAll(path, 0o700))
	path, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	owner := providerregistry.RegistrationOwner{ProviderID: "client-only"}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers:   []config.RemoteProviderDefinition{{Config: config.ProviderConfig{ID: owner.ProviderID, Name: "Client", Type: catalog.TypeOpenAICompat, BaseURL: "https://client.invalid/v1", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Models: []catalog.Model{{ID: "client-model", Name: "Client model", ContextWindow: 8192, DefaultMaxTokens: 1024}}}}},
		Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: owner.ProviderID, Model: "client-model"}, config.SelectedModelTypeSmall: {Provider: owner.ProviderID, Model: "client-model"}},
		Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-private-client-key"}},
	}
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	request := proto.CreateWorkspaceRequest{Workspace: proto.Workspace{Path: path, ClientID: uuid.NewString()}, AuthorityMode: "client", Runtime: &proposal}
	call := func(clientName, method, route string, value any) (int, []byte) {
		t.Helper()
		data, err := json.Marshal(value)
		require.NoError(t, err)
		r, err := http.NewRequestWithContext(t.Context(), method, hs.URL+route, bytes.NewReader(data))
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Crux-Runtime-Protocol", proto.RemoteRuntimeProtocol)
		r.Header.Set(cruxlog.EphemeralStateHeader, "1")
		response, err := clients[clientName].Do(r)
		require.NoError(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NotContains(t, string(body), "synthetic-private")
		return response.StatusCode, body
	}
	status, body := call("retained", http.MethodGet, "/v1/runtime-capabilities", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var capabilities proto.RemoteRuntimeCapabilities
	require.NoError(t, json.Unmarshal(body, &capabilities))
	require.Len(t, capabilities.Principal, 64)
	status, body = call("retained", http.MethodPost, "/v1/workspaces", request)
	require.Equal(t, http.StatusOK, status, string(body))
	var ws proto.Workspace
	require.NoError(t, json.Unmarshal(body, &ws))
	require.NotNil(t, ws.Authority)
	require.Equal(t, capabilities.Principal, ws.Authority.Principal)
	require.Equal(t, filepath.Join(path, ".crux", "remote", capabilities.Principal), ws.DataDir)
	require.Nil(t, ws.Runtime)
	require.Empty(t, ws.ForwardedAccounts)
	require.Empty(t, ws.ForwardedProviders)
	provider, exists := ws.Config.Providers.Get("client-only")
	require.True(t, exists)
	require.False(t, provider.Disable)
	require.Equal(t, "https://client.invalid/v1", provider.BaseURL)
	require.Len(t, provider.Models, 1)
	require.Equal(t, "client-model", provider.Models[0].ID)
	_, err = os.Stat(filepath.Join(ws.DataDir, "crux.db"))
	require.NoError(t, err)
	status, body = call("retained", http.MethodPost, "/v1/workspaces", request)
	require.Equal(t, http.StatusOK, status, string(body))
	var reused proto.Workspace
	require.NoError(t, json.Unmarshal(body, &reused))
	require.Equal(t, ws.ID, reused.ID)
	otherRequest := request
	otherRequest.ClientID = uuid.NewString()
	status, _ = call("revoked", http.MethodPost, "/v1/workspaces", otherRequest)
	require.Equal(t, http.StatusForbidden, status)
	for _, route := range []string{"", "/config", "/sessions", "/channel", "/tasks"} {
		status, _ = call("revoked", http.MethodGet, "/v1/workspaces/"+ws.ID+route, nil)
		require.Equal(t, http.StatusForbidden, status, route)
	}
	status, _ = call("revoked", http.MethodDelete, "/v1/clients/"+request.ClientID, nil)
	require.Equal(t, http.StatusForbidden, status)
	status, body = call("revoked", http.MethodGet, "/v1/workspaces", nil)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, "[]", string(body))
	proposal.Revision = 2
	proposal.Credentials[0].Generation = 2
	proposal.Credentials[0].APIKey = "synthetic-private-rotated-key"
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	status, _ = call("retained", http.MethodPost, "/v1/workspaces", request)
	require.Equal(t, http.StatusConflict, status)
	update := proto.UpdateRemoteRuntimeRequest{ExpectedRevision: 1, Runtime: proposal}
	channel := dialRemoteRuntimeChannel(t, hs.URL, clients["retained"], ws.ID, request.ClientID, ws.Authority)
	defer channel.Close()
	command := proto.WorkspaceChannelFrame{Type: proto.WorkspaceChannelRuntimeReplaceFrame, CommandID: uuid.NewString(), RuntimeReplace: &update}
	ack := sendRemoteRuntimeCommand(t, channel, command)
	require.Equal(t, proto.WorkspaceChannelStatusOK, ack.Status)
	require.NotNil(t, ack.Authority)
	require.Equal(t, uint64(2), ack.Authority.Revision)
	ack = sendRemoteRuntimeCommand(t, channel, command)
	require.Equal(t, proto.WorkspaceChannelStatusConflict, ack.Status)
	require.Equal(t, "duplicate workspace channel command", ack.Message)
	command.CommandID = uuid.NewString()
	ack = sendRemoteRuntimeCommand(t, channel, command)
	require.Equal(t, proto.WorkspaceChannelStatusConflict, ack.Status)
	require.Nil(t, ack.Authority)
	status, body = call("retained", http.MethodGet, "/v1/workspaces/"+ws.ID, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &ws))
	require.Equal(t, uint64(2), ws.Authority.Revision)
}

func remoteServerState(t *testing.T) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_DATA", "CRUX_GLOBAL_CONFIG", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		root := os.Getenv(name)
		require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			value := fmt.Sprintf("%v", info.Mode())
			if !entry.IsDir() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				value += fmt.Sprintf(":%x", sha256.Sum256(data))
			}
			result[path] = value
			return nil
		}))
	}
	return result
}

func TestRemoteRuntimePrivateDecodeRejectsAmbiguityAndBounds(t *testing.T) {
	for _, value := range []string{`{"authority_mode":"client","authority_mode":"server"}`, `{"runtime":{"version":1,"version":2}}`, `{} {}`, strings.Repeat("[", 66) + strings.Repeat("]", 66), `{"unknown_private":"synthetic-private-secret"}`} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces", strings.NewReader(value))
		var request proto.CreateWorkspaceRequest
		err := decodeRuntimeRequest(httptest.NewRecorder(), r, &request)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "synthetic-private")
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces", strings.NewReader(`{}`))
	r.ContentLength = maxRemoteRequestBytes + 1
	require.ErrorContains(t, decodeRuntimeRequest(httptest.NewRecorder(), r, &proto.CreateWorkspaceRequest{}), "byte limit")
	r = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces", strings.NewReader(`{"authority_mode":"server","path":"/workspace"}`))
	require.NoError(t, decodeRuntimeRequest(httptest.NewRecorder(), r, &proto.CreateWorkspaceRequest{}))
}
