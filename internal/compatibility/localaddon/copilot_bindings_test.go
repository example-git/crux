package localaddon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestCopilotSessionBindingsPersistCallerIDs(t *testing.T) {
	workspace := &proto.Workspace{Path: t.TempDir()}
	require.NoError(t, saveCopilotBinding(workspace, "caller-session", "native-session"))

	bindings, err := loadCopilotBindings(workspace)
	require.NoError(t, err)
	require.Equal(t, "native-session", bindings["caller-session"])

	info, err := os.Stat(filepath.Join(workspace.Path, ".crux", "compatibility", "copilot-sessions.json"))
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	require.NoError(t, deleteCopilotBinding(workspace, "caller-session"))
	bindings, err = loadCopilotBindings(workspace)
	require.NoError(t, err)
	require.NotContains(t, bindings, "caller-session")
}

func TestPersistCopilotBindingDeletesSessionOnFailure(t *testing.T) {
	const workspaceID = "workspace-1"
	const sessionID = "session-1"
	deleted := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodDelete, request.Method)
		require.Equal(t, "/v1/workspaces/"+workspaceID+"/sessions/"+sessionID, request.URL.Path)
		deleted <- sessionID
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	api, err := client.NewClient(t.TempDir(), "tcp", serverURL.Host)
	require.NoError(t, err)

	workingDir := t.TempDir()
	blockedDataDir := filepath.Join(workingDir, "blocked")
	require.NoError(t, os.WriteFile(blockedDataDir, []byte("not a directory"), 0o600))
	workspace := &codexNativeWorkspace{
		client: api,
		value:  &proto.Workspace{ID: workspaceID, Path: workingDir, DataDir: blockedDataDir},
	}
	nativeSession := &nativeCompatSession{workspace: workspace, session: &proto.Session{ID: sessionID}}
	bridge := &nativeCompatBridge{ephemeral: map[*nativeCompatSession]struct{}{nativeSession: {}}}

	err = persistCopilotBinding(bridge, nativeSession, "caller-session")
	require.Error(t, err)
	require.Equal(t, sessionID, <-deleted)
	require.NotContains(t, bridge.ephemeral, nativeSession)
}

func TestNativeCreateSessionDeletesSessionWhenModelSelectionFails(t *testing.T) {
	const workspaceID = "workspace-1"
	const sessionID = "session-1"
	selection := config.SelectedModel{Provider: "openai", Model: "gpt-5"}
	owner := providerregistry.RegistrationOwner{ProviderID: selection.Provider, Construction: providerregistry.ConstructionOpenAICompat}
	deleted := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/workspaces/"+workspaceID+"/sessions":
			require.NoError(t, json.NewEncoder(writer).Encode(proto.Session{ID: sessionID}))
		case request.Method == http.MethodPost && request.URL.Path == "/v1/workspaces/"+workspaceID+"/config/model-overrides":
			http.Error(writer, "override failed", http.StatusConflict)
		case request.Method == http.MethodDelete && request.URL.Path == "/v1/workspaces/"+workspaceID+"/sessions/"+sessionID:
			deleted <- sessionID
			writer.WriteHeader(http.StatusOK)
		default:
			http.Error(writer, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	api, err := client.NewClient(t.TempDir(), "tcp", serverURL.Host)
	require.NoError(t, err)

	workingDir := t.TempDir()
	workspace := &codexNativeWorkspace{client: api, value: &proto.Workspace{ID: workspaceID, Path: workingDir}}
	base := &codexNativeBridge{
		workspaces: map[string]*codexNativeWorkspace{workingDir: workspace},
		models: map[*codexNativeWorkspace]map[string]config.SelectedModel{
			workspace: {"openai/gpt-5": selection},
		},
		modelOwners: map[*codexNativeWorkspace]map[string]providerregistry.RegistrationOwner{
			workspace: {selection.Provider: owner},
		},
		selectedModel: map[*codexNativeWorkspace]config.SelectedModel{workspace: selection},
	}
	bridge := &nativeCompatBridge{base: base, ephemeral: make(map[*nativeCompatSession]struct{})}

	session, err := bridge.createSession(context.Background(), workingDir, "Copilot SDK session", "")
	require.Error(t, err)
	require.Nil(t, session)
	require.Equal(t, sessionID, <-deleted)
}
