package server

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBrowserConfinesPathsAndSkipsSymlinks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	child := filepath.Join(root, "child")
	require.NoError(t, os.Mkdir(child, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("ok"), 0o600))
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))

	server := NewServer(nil, "tcp", "0.0.0.0:9090")
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	server.workspaceRoots = []string{canonicalRoot}
	listing, err := server.browse(root)
	require.NoError(t, err)
	require.Equal(t, canonicalRoot, listing.Path)
	require.Empty(t, listing.Parent)
	require.Len(t, listing.Entries, 2)
	require.Equal(t, "child", listing.Entries[0].Name)
	require.True(t, listing.Entries[0].Directory)
	require.Equal(t, "file.txt", listing.Entries[1].Name)

	childListing, err := server.browse(child)
	require.NoError(t, err)
	require.Equal(t, canonicalRoot, childListing.Parent)
	_, err = server.browse(outside)
	require.ErrorContains(t, err, "outside configured workspace roots")
	_, err = server.browse(filepath.Join(root, "escape"))
	require.ErrorContains(t, err, "outside configured workspace roots")
	_, err = server.browse(filepath.Join(root, "file.txt"))
	require.ErrorContains(t, err, "not a directory")
}

func TestBrowserUsesMostSpecificWorkspaceRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	require.NoError(t, os.Mkdir(root, 0o700))
	server := NewServer(nil, "tcp", "0.0.0.0:9090")
	require.NoError(t, server.SetWorkspaceRoots([]string{parent, root}))

	listing, err := server.browse(root)
	require.NoError(t, err)
	require.Empty(t, listing.Parent)
}

func TestOpenedWorkspaceDirectoryIsRejectedAfterPathSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	require.NoError(t, os.Mkdir(root, 0o700))
	outside := t.TempDir()
	directory, err := os.Open(root)
	require.NoError(t, err)
	defer directory.Close()
	require.NoError(t, os.Rename(root, filepath.Join(parent, "moved")))
	require.NoError(t, os.Symlink(outside, root))

	err = validateOpenedWorkspaceDirectory(directory, root, root)
	require.Error(t, err)
}

func TestBrowserListingIsBoundedAndSorted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	for i := 0; i < maxBrowserEntries+10; i++ {
		require.NoError(t, os.Mkdir(filepath.Join(root, fmt.Sprintf("dir-%04d", maxBrowserEntries+10-i)), 0o700))
	}
	server := NewServer(nil, "unix", "test")
	require.NoError(t, server.SetWorkspaceRoots([]string{root}))
	listing, err := server.browse(root)
	require.NoError(t, err)
	require.True(t, listing.Truncated)
	require.Len(t, listing.Entries, maxBrowserEntries)
	for i := 1; i < len(listing.Entries); i++ {
		require.Less(t, listing.Entries[i-1].Name, listing.Entries[i].Name)
	}
}

func TestRemoteWorkspacePathsAreRejectedBeforeBackendAccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	outside := t.TempDir()
	server := NewServer(nil, "tcp", "0.0.0.0:9090")
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	server.workspaceRoots = []string{canonicalRoot}
	controller := &controllerV1{backend: server.backend, server: server}
	body := []byte(fmt.Sprintf(`{"path":%q,"data_dir":%q,"client_id":"00000000-0000-4000-8000-000000000001"}`, outside, filepath.Join(outside, ".crux")))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces", bytes.NewReader(body))
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	response := httptest.NewRecorder()
	controller.handlePostWorkspaces(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Empty(t, server.backend.ListWorkspaces())
}

func TestHTTPServerUsesBoundedHeaderAndIdleTimeouts(t *testing.T) {
	server := NewServer(nil, "tcp", "127.0.0.1:9090")
	require.Positive(t, server.h.ReadHeaderTimeout)
	require.Positive(t, server.h.IdleTimeout)
	require.Positive(t, server.h.MaxHeaderBytes)
	require.Zero(t, server.h.WriteTimeout)
}

func TestRemoteManagementRequiresAuthenticatedTLS(t *testing.T) {
	server := NewServer(nil, "tcp", "0.0.0.0:9090")
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/browser", nil)
	require.False(t, server.authenticatedManagementRequest(request))
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	require.True(t, server.authenticatedManagementRequest(request))
}
