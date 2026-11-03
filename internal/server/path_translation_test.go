package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/proto"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPathTranslationRejectsInvalidSettingsBeforeBackend(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"scope":{"kind":"project"}}`,
		`{"scope":{"kind":"provider","provider":"available"}}`,
		`{"scope":{"kind":"model","provider":"available","model":"available"}}`,
		`{"scope":{"kind":"session","session_id":"one"},"settings":{"enabled":true,"targets":[{"path":"relative"}]}}`,
		`{"scope":{"kind":"session","session_id":"one"},"settings":{"inherit":true,"enabled":true}}`,
		`{"scope":{"kind":"session","session_id":"one"},"secret":"not-allowed"}`,
		`{"scope":{"kind":"session","session_id":"one"}} {}`,
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
		response := httptest.NewRecorder()
		(&controllerV1{}).handlePostWorkspacePathTranslation(response, request)
		require.Equal(t, http.StatusBadRequest, response.Code, body)
	}
}

func TestPathTranslationHTTPPersistsIsolatedSessionPolicy(t *testing.T) {
	h := newRealCreateHarness(t)
	t.Cleanup(h.backend.Shutdown)
	workspace := h.postWorkspace(t, proto.Workspace{Path: t.TempDir(), DataDir: t.TempDir(), ClientID: uuid.NewString()})
	one, err := h.backend.CreateSession(t.Context(), workspace.ID, "protected")
	require.NoError(t, err)
	two, err := h.backend.CreateSession(t.Context(), workspace.ID, "plain")
	require.NoError(t, err)
	policy := pathtranslate.Policy{
		Scope: pathtranslate.Scope{Kind: "session", SessionID: one.ID},
		Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{
			{Path: "/unvisited/large-tree", Label: "Library", Recursive: true, CensorImages: true},
		}},
	}
	body, err := json.Marshal(policy)
	require.NoError(t, err)
	endpoint := h.httpSrv.URL + "/v1/workspaces/" + workspace.ID + "/path-translation"
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	response, err := h.httpSrv.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var saved pathtranslate.Policy
	require.NoError(t, json.NewDecoder(response.Body).Decode(&saved))
	require.Equal(t, policy.Scope, saved.Scope)
	require.Equal(t, policy.Settings, saved.Settings)
	require.EqualValues(t, 1, saved.Revision)
	for _, sessionID := range []string{one.ID, two.ID} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint+"?kind=session&session_id="+url.QueryEscape(sessionID), nil)
		require.NoError(t, err)
		response, err := h.httpSrv.Client().Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		var loaded pathtranslate.Policy
		require.NoError(t, json.NewDecoder(response.Body).Decode(&loaded))
		require.NoError(t, response.Body.Close())
		if sessionID == one.ID {
			require.Equal(t, saved, loaded)
		} else {
			require.False(t, loaded.Settings.Enabled)
			require.False(t, loaded.Settings.Inherit)
		}
	}
}

func TestPathTranslationTLSRequiresCurrentClientAuthority(t *testing.T) {
	server, clients := newRemoteAuthorityTLSHarness(t)
	proposal := synthOwnedProviderProposal(t, "privacy-client")
	call := func(clientName, method, route string, value any, attachment *proto.WorkspaceAttachment) (int, []byte) {
		t.Helper()
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), method, server.URL+route, bytes.NewReader(encoded))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Crux-Runtime-Protocol", proto.RemoteRuntimeProtocol)
		request.Header.Set(cruxlog.EphemeralStateHeader, "1")
		if attachment != nil {
			attachment.SetHeaders(request.Header)
		}
		response, err := clients[clientName].Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, body
	}
	root := filepath.Join(filepath.Dir(os.Getenv("HOME")), "path-translation-workspace")
	require.NoError(t, os.MkdirAll(root, 0o700))
	status, body := call("retained", http.MethodPost, "/v1/workspaces", proto.CreateWorkspaceRequest{Workspace: proto.Workspace{Path: root, ClientID: uuid.NewString()}, AuthorityMode: "client", Runtime: &proposal}, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var workspace proto.Workspace
	require.NoError(t, json.Unmarshal(body, &workspace))
	require.NotNil(t, workspace.Authority)
	attachment := &proto.WorkspaceAttachment{Mode: "client", Revision: workspace.Authority.Revision, Digest: workspace.Authority.Digest}
	status, body = call("retained", http.MethodPost, "/v1/workspaces/"+workspace.ID+"/sessions", map[string]string{"title": "private session"}, attachment)
	require.Equal(t, http.StatusOK, status, string(body))
	var current proto.Session
	require.NoError(t, json.Unmarshal(body, &current))
	require.NotEmpty(t, current.ID)
	route := "/v1/workspaces/" + workspace.ID + "/path-translation"
	policy := pathtranslate.Policy{Scope: pathtranslate.Scope{Kind: "session", SessionID: current.ID}, Settings: pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: "/private/selected", Recursive: true}}}}
	status, body = call("retained", http.MethodPost, route, policy, nil)
	require.Equal(t, http.StatusConflict, status, string(body))
	status, body = call("retained", http.MethodPost, route, policy, attachment)
	require.Equal(t, http.StatusOK, status, string(body))
	var saved pathtranslate.Policy
	require.NoError(t, json.Unmarshal(body, &saved))
	require.EqualValues(t, 1, saved.Revision)
	stale := *attachment
	stale.Revision++
	status, body = call("retained", http.MethodPost, route, saved, &stale)
	require.Equal(t, http.StatusConflict, status, string(body))
	status, body = call("revoked", http.MethodPost, route, saved, attachment)
	require.Equal(t, http.StatusForbidden, status, string(body))
	status, body = call("retained", http.MethodGet, route+"?kind=session&session_id="+url.QueryEscape(current.ID), nil, attachment)
	require.Equal(t, http.StatusOK, status, string(body))
	var loaded pathtranslate.Policy
	require.NoError(t, json.Unmarshal(body, &loaded))
	require.Equal(t, saved, loaded)
}
