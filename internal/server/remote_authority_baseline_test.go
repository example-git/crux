package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func newRemoteAuthorityTLSHarness(t *testing.T) (*httptest.Server, map[string]*http.Client) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_DATA", "CRUX_GLOBAL_CONFIG", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		t.Setenv(name, filepath.Join(root, name))
		require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o700))
	}
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	clients := make(map[string]*http.Client)
	for _, name := range []string{"revoked", "retained", "unauthorized"} {
		saved, code, err := connection.Add(t.Context(), name, "tcp://127.0.0.1:9443", serverCode)
		require.NoError(t, err)
		if name != "unauthorized" {
			require.NoError(t, connection.AuthorizeClient(t.Context(), name, code))
		}
		cfg, err := connection.ClientTLSConfig(saved)
		require.NoError(t, err)
		cfg.ClientSessionCache = tls.NewLRUClientSessionCache(4)
		transport := &http.Transport{TLSClientConfig: cfg}
		t.Cleanup(transport.CloseIdleConnections)
		clients[name] = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	}
	srv := NewServer(nil, "tcp", "127.0.0.1:0")
	t.Cleanup(srv.backend.Shutdown)
	require.NoError(t, srv.SetWorkspaceRoots([]string{root}))
	require.NoError(t, srv.EnableNetworkAuth(t.Context()))
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.TLS = srv.tlsConfig
	hs.StartTLS()
	t.Cleanup(hs.Close)
	return hs, clients
}

func TestRemoteTLSAdmissionBaseline(t *testing.T) {
	hs, clients := newRemoteAuthorityTLSHarness(t)
	response, err := clients["unauthorized"].Get(hs.URL + "/v1/workspaces")
	if response != nil {
		response.Body.Close()
	}
	require.Error(t, err, "an unapproved actual TLS certificate must not reach the API")
	response, err = clients["retained"].Post(hs.URL+"/v1/enroll", "application/json", bytes.NewBufferString(`{}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
	for _, kind := range []string{"providers", "accounts", "mixed"} {
		for _, marked := range []bool{false, true} {
			t.Run(kind+"/marked="+map[bool]string{false: "false", true: "true"}[marked], func(t *testing.T) {
				// A missing client ID intentionally stops this probe immediately
				// after the forwarding boundary, before config or DB initialization.
				args := proto.Workspace{Path: t.TempDir()}
				if kind != "accounts" {
					args.ForwardedProviders = map[string]config.ProviderConfig{"example": {ID: "example", APIKey: "synthetic-private-key"}}
				}
				if kind != "providers" {
					args.ForwardedAccounts = map[string]config.ForwardedAccount{"example": {Entry: accounts.Entry{ID: "client", AccessToken: "synthetic-private-access"}}}
				}
				body, err := json.Marshal(args)
				require.NoError(t, err)
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, hs.URL+"/v1/workspaces", bytes.NewReader(body))
				require.NoError(t, err)
				if marked {
					request.Header.Set(cruxlog.EphemeralStateHeader, "1")
				}
				response, err := clients["retained"].Do(request)
				require.NoError(t, err)
				defer response.Body.Close()
				content, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Equal(t, http.StatusBadRequest, response.StatusCode)
				if marked {
					require.Contains(t, string(content), "client_id")
				} else {
					require.Contains(t, string(content), "must be marked ephemeral")
				}
				require.NotContains(t, string(content), "synthetic-private")
			})
		}
	}
}

// A5 records the same-running-daemon gap, including resumption and keepalive.
// F4 will invert these expectations after live authorization is implemented.
func TestRemoteTLSRevocationBaseline(t *testing.T) {
	hs, clients := newRemoteAuthorityTLSHarness(t)
	probe := func(client *http.Client) (bool, bool) {
		t.Helper()
		var reused, resumed bool
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
		require.NoError(t, err)
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}))
		response, err := client.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		if response.TLS != nil {
			resumed = response.TLS.DidResume
		}
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		return reused, resumed
	}
	probe(clients["revoked"])
	probe(clients["retained"])
	require.NoError(t, connection.RevokeClient(t.Context(), "revoked"))
	reused, _ := probe(clients["revoked"])
	require.True(t, reused, "must exercise a connection established before revoke")
	transport := clients["revoked"].Transport.(*http.Transport)
	transport.CloseIdleConnections()
	_, resumed := probe(clients["revoked"])
	require.True(t, resumed, "must exercise TLS session resumption after revoke")
	fresh := transport.Clone()
	fresh.TLSClientConfig.ClientSessionCache = nil
	t.Cleanup(fresh.CloseIdleConnections)
	_, resumed = probe(&http.Client{Transport: fresh, Timeout: 5 * time.Second})
	require.False(t, resumed, "must also exercise a full new handshake after revoke")
	probe(clients["retained"])
}
