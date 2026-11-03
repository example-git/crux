package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRemoteProjectSessionsWithoutWorkspaceAttachment(t *testing.T) {
	native := NewServer(nil, "tcp", "0.0.0.0:9090")
	t.Cleanup(func() { _ = native.Close() })
	root := t.TempDir()
	require.NoError(t, native.SetWorkspaceRoots([]string{root}))
	controller := &controllerV1{backend: native.backend, server: native}
	for _, title := range []string{"first", "second"} {
		path := filepath.Join(root, title)
		require.NoError(t, os.Mkdir(path, 0o700))
		dataDir := filepath.Join(path, ".crux")
		conn, err := db.Connect(t.Context(), dataDir)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.ReleaseConnection(conn)) })
		created, err := session.NewService(db.New(conn), conn).Create(t.Context(), title)
		require.NoError(t, err)
		query := url.Values{"path": {path}, "data_dir": {dataDir}}
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/project-sessions?"+query.Encode(), nil)
		request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
		response := httptest.NewRecorder()
		controller.handleGetProjectSessions(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var result session.Catalog
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Len(t, result.Sessions, 1)
		require.Equal(t, created.ID, result.Sessions[0].ID)
		require.Equal(t, title, result.Sessions[0].Title)
	}
	require.Empty(t, native.backend.ListWorkspaces())
}

func TestRemoteProjectSessionsConfinesPathsAndRequiresAuthentication(t *testing.T) {
	native := NewServer(nil, "tcp", "0.0.0.0:9090")
	t.Cleanup(func() { _ = native.Close() })
	root, outside := t.TempDir(), t.TempDir()
	require.NoError(t, native.SetWorkspaceRoots([]string{root}))
	controller := &controllerV1{backend: native.backend, server: native}
	for _, test := range []struct {
		name          string
		path          string
		dataDir       string
		authenticated bool
		status        int
	}{
		{"unauthenticated", root, "", false, http.StatusForbidden},
		{"outside project", outside, "", true, http.StatusBadRequest},
		{"outside data", root, outside, true, http.StatusBadRequest},
		{"empty catalog", root, filepath.Join(root, ".crux"), true, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := url.Values{"path": {test.path}, "data_dir": {test.dataDir}}
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/project-sessions?"+query.Encode(), nil)
			if test.authenticated {
				request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
			}
			response := httptest.NewRecorder()
			controller.handleGetProjectSessions(response, request)
			require.Equal(t, test.status, response.Code, response.Body.String())
		})
	}
	require.NoDirExists(t, filepath.Join(root, ".crux"))
	require.Empty(t, native.backend.ListWorkspaces())
}
