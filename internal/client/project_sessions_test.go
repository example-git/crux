package client

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestProjectSessionsPreservesSelectionAndPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/project-sessions", r.URL.Path)
		require.Equal(t, "/srv/project with spaces", r.URL.Query().Get("path"))
		require.Equal(t, "/srv/data", r.URL.Query().Get("data_dir"))
		require.Equal(t, "100", r.URL.Query().Get("offset"))
		jsonEncodeTest(t, w, session.Catalog{Sessions: []session.CatalogEntry{{ID: "remote", Title: "Remote session"}}})
	}))
	defer server.Close()
	client := captureClient(t, server)
	result, err := client.ProjectSessions(t.Context(), "/srv/project with spaces", "/srv/data", 100)
	require.NoError(t, err)
	require.Len(t, result.Sessions, 1)
	require.Equal(t, "remote", result.Sessions[0].ID)
}

func TestProjectSessionsReportsUnsupportedServer(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	_, err := captureClient(t, server).ProjectSessions(t.Context(), "/srv/project", "", 0)
	require.Error(t, err)
}
