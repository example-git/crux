package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/lock"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func newRemoteAuthorityTLSHarness(t *testing.T, configure ...func(*tls.Config)) (*httptest.Server, map[string]*http.Client) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_DATA", "CRUX_GLOBAL_CONFIG", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		t.Setenv(name, filepath.Join(root, name))
		require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o700))
	}
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	clients := make(map[string]*http.Client)
	var principals []string
	for _, name := range []string{"revoked", "retained", "unauthorized"} {
		saved, code, err := connection.Add(t.Context(), name, "tcp://127.0.0.1:9443", serverCode)
		require.NoError(t, err)
		if name != "unauthorized" {
			require.NoError(t, connection.AuthorizeClient(t.Context(), name, code))
		}
		cfg, err := connection.ClientTLSConfig(saved)
		require.NoError(t, err)
		fingerprint := sha256.Sum256(cfg.Certificates[0].Certificate[0])
		principals = append(principals, hex.EncodeToString(fingerprint[:]))
		cfg.ClientSessionCache = tls.NewLRUClientSessionCache(4)
		transport := &http.Transport{TLSClientConfig: cfg}
		t.Cleanup(transport.CloseIdleConnections)
		clients[name] = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	}
	srv := NewServer(nil, "tcp", "127.0.0.1:0")
	t.Cleanup(func() {
		// Backend.Shutdown initiates HTTP shutdown; it does not drain the
		// workspaces created by this fixture. Join each principal's retained
		// lifetime, including workspaces already removed from the public list
		// by automatic retirement, before removing temporary state.
		for _, principal := range principals {
			require.NoError(t, srv.backend.RevokePrincipal(context.Background(), principal))
		}
		srv.backend.Shutdown()
	})
	require.NoError(t, srv.SetWorkspaceRoots([]string{root}))
	require.NoError(t, srv.EnableNetworkAuth(t.Context()))
	for _, apply := range configure {
		apply(srv.tlsConfig)
	}
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.TLS = srv.tlsConfig
	hs.StartTLS()
	t.Cleanup(hs.Close)
	return hs, clients
}

func TestRemoteTLSAdmissionBaseline(t *testing.T) {
	hs, clients := newRemoteAuthorityTLSHarness(t)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
	require.NoError(t, err)
	response, err := clients["unauthorized"].Do(request)
	if response != nil {
		response.Body.Close()
	}
	require.Error(t, err, "an unapproved actual TLS certificate must not reach the API")
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, hs.URL+"/v1/enroll", bytes.NewBufferString(`{}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err = clients["retained"].Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
	for _, kind := range []string{"providers", "accounts", "mixed"} {
		for _, marked := range []bool{false, true} {
			t.Run(kind+"/marked="+map[bool]string{false: "false", true: "true"}[marked], func(t *testing.T) {
				// Legacy forwarding is now rejected at the boundary, before
				// config or DB initialization, even for an authenticated peer.
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
					require.Contains(t, string(content), "legacy provider forwarding is unsupported")
				} else {
					require.Contains(t, string(content), "must be marked ephemeral")
				}
				require.NotContains(t, string(content), "synthetic-private")
			})
		}
	}
}

type observedRemoteSessionCache struct {
	tls.ClientSessionCache
	hits atomic.Int64
}

func (c *observedRemoteSessionCache) Get(key string) (*tls.ClientSessionState, bool) {
	state, found := c.ClientSessionCache.Get(key)
	if found && state != nil {
		c.hits.Add(1)
	}
	return state, found
}

// Current authorization applies without rebuilding the same running server.
func TestRemoteTLSLiveRevocation(t *testing.T) {
	hs, clients := newRemoteAuthorityTLSHarness(t)
	transport := clients["revoked"].Transport.(*http.Transport)
	cache := &observedRemoteSessionCache{ClientSessionCache: transport.TLSClientConfig.ClientSessionCache}
	transport.TLSClientConfig.ClientSessionCache = cache
	probe := func(client *http.Client, route string, wantStatus int) (bool, bool, error) {
		t.Helper()
		var reused, resumed bool
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+route, nil)
		require.NoError(t, err)
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}))
		response, err := client.Do(request)
		if err != nil {
			return reused, resumed, err
		}
		require.Equal(t, wantStatus, response.StatusCode)
		if response.TLS != nil {
			resumed = response.TLS.DidResume
		}
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		if wantStatus == http.StatusForbidden {
			require.Contains(t, string(body), connection.ErrClientAuthorization.Error())
			require.NotContains(t, string(body), "connections.json")
		}
		return reused, resumed, nil
	}
	_, _, err := probe(clients["revoked"], "/v1/workspaces", http.StatusOK)
	require.NoError(t, err)
	_, _, err = probe(clients["retained"], "/v1/workspaces", http.StatusOK)
	require.NoError(t, err)
	// Establish that this fixture actually supports session resumption.
	retainedTransport := clients["retained"].Transport.(*http.Transport)
	retainedTransport.CloseIdleConnections()
	_, resumed, err := probe(clients["retained"], "/v1/workspaces", http.StatusOK)
	require.NoError(t, err)
	require.True(t, resumed)
	require.NoError(t, connection.RevokeClient(t.Context(), "revoked"))
	for _, route := range []string{"/v1/workspaces", "/v1/workspaces/missing", "/v1/runtime-capabilities", "/v1/docs/index.html", "/not-a-route"} {
		reused, _, err := probe(clients["revoked"], route, http.StatusForbidden)
		require.NoError(t, err)
		require.True(t, reused, "must exercise the connection established before revoke")
	}
	cacheHits := cache.hits.Load()
	transport.CloseIdleConnections()
	_, _, err = probe(clients["revoked"], "/v1/workspaces", http.StatusOK)
	require.Error(t, err, "revoked session must fail TLS verification before reaching HTTP")
	require.Greater(t, cache.hits.Load(), cacheHits, "the denied handshake must attempt to reuse its real cached TLS session")
	fresh := transport.Clone()
	fresh.TLSClientConfig.ClientSessionCache = nil
	t.Cleanup(fresh.CloseIdleConnections)
	_, _, err = probe(&http.Client{Transport: fresh, Timeout: 5 * time.Second}, "/v1/workspaces", http.StatusOK)
	require.Error(t, err, "a fresh handshake must reject the revoked certificate too")
	_, _, err = probe(clients["retained"], "/v1/workspaces", http.StatusOK)
	require.NoError(t, err)
	retainedTransport.CloseIdleConnections()
	_, resumed, err = probe(clients["retained"], "/v1/workspaces", http.StatusOK)
	require.NoError(t, err)
	require.True(t, resumed, "revocation must preserve nonrevoked session resumption")
}

func TestRemoteTLSRevocationDuringResumedHandshake(t *testing.T) {
	var revokeAfterClientHello atomic.Bool
	var deniedResumptions atomic.Int64
	hs, clients := newRemoteAuthorityTLSHarness(t, func(cfg *tls.Config) {
		getConfig := cfg.GetConfigForClient
		cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			candidate, err := getConfig(hello)
			if err != nil {
				return nil, err
			}
			if revokeAfterClientHello.Swap(false) {
				// Trust was read while this client was authorized. Revoke at
				// the actual handshake boundary before VerifyConnection runs.
				if err := connection.RevokeClient(hello.Context(), "revoked"); err != nil {
					return nil, err
				}
			}
			verify := candidate.VerifyConnection
			candidate.VerifyConnection = func(state tls.ConnectionState) error {
				err := verify(state)
				if state.DidResume && err != nil {
					deniedResumptions.Add(1)
				}
				return err
			}
			return candidate, nil
		}
	})
	client := clients["revoked"]
	for attempt := range 2 {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.Equal(t, attempt > 0, response.TLS.DidResume)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		client.Transport.(*http.Transport).CloseIdleConnections()
	}
	revokeAfterClientHello.Store(true)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
	require.NoError(t, err)
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
	}
	require.Error(t, err)
	require.EqualValues(t, 1, deniedResumptions.Load(), "VerifyConnection must reject the actually resumed session using current grants")
}

func TestRemoteTLSAuthorizationUsesCapturedStore(t *testing.T) {
	hs, clients := newRemoteAuthorityTLSHarness(t)
	originalPath := filepath.Join(config.GlobalWorkspaceDir(), "connections.json")
	get := func(client *http.Client, want int) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, want, response.StatusCode)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
	}
	get(clients["revoked"], http.StatusOK)
	get(clients["retained"], http.StatusOK)
	original, err := os.ReadFile(originalPath)
	require.NoError(t, err)
	otherRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(otherRoot, "connections.json"), original, 0o600))
	t.Setenv("CRUX_GLOBAL_DATA", otherRoot)
	// Changing process environment must not move the server's authority.
	require.NoError(t, connection.RevokeClient(t.Context(), "retained"))
	get(clients["retained"], http.StatusOK)
	clients["retained"].Transport.(*http.Transport).CloseIdleConnections()
	get(clients["retained"], http.StatusOK)
	var current map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(original, &current))
	var authorized map[string]string
	require.NoError(t, json.Unmarshal(current["authorized_clients"], &authorized))
	delete(authorized, "revoked")
	current["authorized_clients"], err = json.Marshal(authorized)
	require.NoError(t, err)
	data, err := json.Marshal(current)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(originalPath, data, 0o600))
	get(clients["revoked"], http.StatusForbidden)
	clients["revoked"].Transport.(*http.Transport).CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
	require.NoError(t, err)
	response, err := clients["revoked"].Do(request)
	if response != nil {
		response.Body.Close()
	}
	require.Error(t, err, "TLS must also keep using the original captured path")
}

func TestRemoteTLSAuthorizationMalformedStoreFailsClosed(t *testing.T) {
	hs, clients := newRemoteAuthorityTLSHarness(t)
	path := filepath.Join(config.GlobalWorkspaceDir(), "connections.json")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	mutate := func(action func()) {
		release, err := lock.File(t.Context(), path+".lock")
		require.NoError(t, err)
		defer release()
		action()
	}
	for _, damage := range []string{"invalid-json", "trailing-json", "duplicate-key", "case-alias-clients", "invalid-utf8", "invalid-server-key", "invalid-client-certificate", "directory", "missing"} {
		t.Run(damage, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
			require.NoError(t, err)
			response, err := clients["retained"].Do(request)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			mutate(func() {
				switch damage {
				case "invalid-json":
					require.NoError(t, os.WriteFile(path, []byte(`{"secret-client":"synthetic-private-data"`), 0o600))
				case "trailing-json":
					require.NoError(t, os.WriteFile(path, append(bytes.Clone(original), []byte(` {}`)...), 0o600))
				case "duplicate-key":
					require.NoError(t, os.WriteFile(path, bytes.Replace(original, []byte(`"version": 1`), []byte(`"version": 1, "version": 1`), 1), 0o600))
				case "invalid-utf8":
					require.NoError(t, os.WriteFile(path, append(bytes.Clone(original), 0xff), 0o600))
				case "case-alias-clients", "invalid-server-key", "invalid-client-certificate":
					var damaged map[string]any
					require.NoError(t, json.Unmarshal(original, &damaged))
					switch damage {
					case "case-alias-clients":
						clients := damaged["authorized_clients"].(map[string]any)
						damaged["AUTHORIZED_CLIENTS"] = map[string]any{"revoked": clients["revoked"]}
						delete(clients, "revoked")
					case "invalid-server-key":
						damaged["server"].(map[string]any)["private_key"] = "synthetic-private-data"
					default:
						damaged["authorized_clients"].(map[string]any)["secret-client"] = "synthetic-private-data"
					}
					data, err := json.Marshal(damaged)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(path, data, 0o600))
				case "directory", "missing":
					require.NoError(t, os.Remove(path))
					if damage == "directory" {
						require.NoError(t, os.Mkdir(path, 0o700))
					}
				}
			})
			request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
			require.NoError(t, err)
			response, err = clients["retained"].Do(request)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, response.StatusCode)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Contains(t, string(body), connection.ErrClientAuthorization.Error())
			require.NotContains(t, string(body), path)
			require.NotContains(t, string(body), "secret-client")
			require.NotContains(t, string(body), "synthetic-private-data")
			clients["retained"].Transport.(*http.Transport).CloseIdleConnections()
			request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
			require.NoError(t, err)
			response, err = clients["retained"].Do(request)
			if response != nil {
				response.Body.Close()
			}
			require.Error(t, err, "malformed state must also deny a new TLS handshake")
			if damage == "case-alias-clients" {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
				require.NoError(t, err)
				response, err = clients["revoked"].Do(request)
				if response != nil {
					response.Body.Close()
				}
				require.Error(t, err, "a case-aliased grant must not restore the removed client")
			}
			mutate(func() {
				if damage == "directory" {
					require.NoError(t, os.Remove(path))
				}
				require.NoError(t, os.WriteFile(path, original, 0o600))
			})
		})
	}
}

func TestRemoteTLSNewAuthorizationUsesCurrentTrust(t *testing.T) {
	hs, clients := newRemoteAuthorityTLSHarness(t)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
	require.NoError(t, err)
	response, err := clients["unauthorized"].Do(request)
	if response != nil {
		response.Body.Close()
	}
	require.Error(t, err)
	saved, exists, err := connection.Get(t.Context(), "unauthorized")
	require.NoError(t, err)
	require.True(t, exists)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "now-authorized", saved.Client.Certificate))
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces", nil)
	require.NoError(t, err)
	response, err = clients["unauthorized"].Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
}
