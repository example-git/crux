package webclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestProjectCatalogReadsSeparateDatabasesWithoutWorkspaces(t *testing.T) {
	native := server.NewServer(nil, "tcp", "127.0.0.1:9090")
	t.Cleanup(func() { _ = native.Close() })
	handler := Handler(native, "127.0.0.1:9090", Startup{})
	for _, title := range []string{"First project", "Second project"} {
		root := t.TempDir()
		dataDir := filepath.Join(root, ".crux")
		conn, err := db.Connect(t.Context(), dataDir)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.ReleaseConnection(conn)) })
		created, err := session.NewService(db.New(conn), conn).Create(t.Context(), title)
		require.NoError(t, err)
		params := url.Values{"path": {root}, "data_dir": {dataDir}}
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:9090/ui/project-sessions?"+params.Encode(), nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var result struct {
			Sessions []projectSession `json:"sessions"`
			More     bool             `json:"more"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Len(t, result.Sessions, 1)
		require.Equal(t, created.ID, result.Sessions[0].ID)
		require.Equal(t, title, result.Sessions[0].Title)
		require.False(t, result.More)
	}
	require.Empty(t, native.Backend().ListWorkspaces())
}

func TestProjectCatalogDoesNotCreateMissingDatabase(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, ".crux")
	params := url.Values{"path": {root}, "data_dir": {dataDir}}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/project-sessions?"+params.Encode(), nil)
	response := httptest.NewRecorder()
	projectSessions(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.JSONEq(t, `{"sessions":[],"more":false}`, response.Body.String())
	require.NoDirExists(t, dataDir)
	params.Set("offset", "-1")
	response = httptest.NewRecorder()
	projectSessions(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/project-sessions?"+params.Encode(), nil))
	require.Equal(t, http.StatusBadRequest, response.Code)
}
