package webclient

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type settingsWorkspace struct {
	workspace.Workspace
	scope config.Scope
	key   string
	value any
	calls int
}

func (s *settingsWorkspace) SetConfigField(scope config.Scope, key string, value any) error {
	s.scope, s.key, s.value = scope, key, value
	s.calls++
	return nil
}

func TestWorkspaceSettingsReuseNativeRequestHandler(t *testing.T) {
	native := server.NewServer(nil, "tcp", "127.0.0.1:9090")
	t.Cleanup(func() { _ = native.Close() })
	selected := &settingsWorkspace{}
	handler := native.WorkspaceSettingsHandler(workspaceSettings{resolve: func(id string) (workspace.Workspace, error) {
		if id != "remote" {
			return nil, nil
		}
		return selected, nil
	}})
	body, err := json.Marshal(proto.ConfigSetRequest{Scope: config.ScopeGlobal, Key: "options.disabled_tools", Value: []string{"download"}})
	require.NoError(t, err)
	for _, id := range []string{"remote", "missing"} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/"+id+"/config/set", bytes.NewReader(body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if id == "remote" {
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		} else {
			require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
		}
	}
	require.Equal(t, 1, selected.calls)
	require.Equal(t, config.ScopeGlobal, selected.scope)
	require.Equal(t, "options.disabled_tools", selected.key)
	require.Equal(t, []any{"download"}, selected.value)
	require.Empty(t, native.Backend().ListWorkspaces())
}

func TestWorkspaceSettingsMissingAdapterDoesNotUseServerBackend(t *testing.T) {
	native := server.NewServer(nil, "tcp", "127.0.0.1:9090")
	t.Cleanup(func() { _ = native.Close() })
	response := httptest.NewRecorder()
	native.WorkspaceSettingsHandler(nil).ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/remote/config/set", nil))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
}
