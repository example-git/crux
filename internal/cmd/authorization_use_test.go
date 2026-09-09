package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/connection"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestConnectionsAuthorizedReportsLiveUseWithoutPersistence(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", root)
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	saved, certificate, err := connection.Add(t.Context(), "live-cli", "tcp://127.0.0.1:1", serverCode)
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "live-cli", certificate))
	serverTLS, authority, err := connection.ServerTLSConfigWithAuthorization(t.Context())
	require.NoError(t, err)
	require.NoError(t, authority.StartLive(t.Context(), func(string) error { return nil }, func(context.Context, string) error { return nil }))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, authority.CloseLive(ctx))
	}()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, done, err := authority.AdmitRequest(r.Context(), *r.TLS, nil)
		if err != nil {
			http.Error(w, "not authorized", http.StatusForbidden)
			return
		}
		defer done()
		w.WriteHeader(http.StatusNoContent)
	}))
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	clientTLS, err := connection.ClientTLSConfig(saved)
	require.NoError(t, err)
	transport := &http.Transport{TLSClientConfig: clientTLS, Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Get(server.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	require.NoError(t, response.Body.Close())
	type entry struct {
		Mode     fs.FileMode
		Size     int64
		Modified time.Time
		Digest   [32]byte
	}
	tree := func() map[string]entry {
		result := map[string]entry{}
		require.NoError(t, filepath.WalkDir(root, func(path string, item fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := item.Info()
			if err != nil {
				return err
			}
			value := entry{Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime()}
			if !item.IsDir() {
				contents, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				value.Digest = sha256.Sum256(contents)
			}
			result[path] = value
			return nil
		}))
		return result
	}
	before := tree()
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetContext(t.Context())
	command.SetOut(&output)
	require.NoError(t, connectionsAuthorizedCmd.RunE(command, nil))
	records, err := connection.ListAuthorizationRecords(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.NotNil(t, records[0].LiveLastUsedAt)
	require.Nil(t, records[0].LastUsedAt)
	require.Contains(t, output.String(), "live-cli")
	require.Contains(t, output.String(), "last-use=unknown\tlive-last-use="+authorizationRecordTime(records[0].LiveLastUsedAt))
	require.Contains(t, output.String(), "live-use=observed")
	require.Contains(t, output.String(), "lost when a daemon exits")
	require.Equal(t, before, tree())
	require.NoError(t, authority.CloseLive(t.Context()))
	output.Reset()
	require.NoError(t, connectionsAuthorizedCmd.RunE(command, nil))
	require.Contains(t, output.String(), "live-last-use=unknown\tlive-use=no-live-daemons")
}
