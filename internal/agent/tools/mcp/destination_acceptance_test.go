package mcp

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func trustMCPResourceServers(t *testing.T, servers ...*httptest.Server) {
	t.Helper()
	previous := http.DefaultTransport
	transport := servers[0].Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	for _, server := range servers {
		transport.TLSClientConfig.RootCAs.AddCert(server.Certificate())
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
}

func closeMCPDestinationManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, manager.Close(ctx))
}

func TestMCPResourceCredentialRedirectsThroughManagerHTTPS(t *testing.T) {
	for _, kind := range []string{"http", "sse"} {
		for _, useOAuth := range []bool{false, true} {
			for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
				for _, external := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/oauth=%t/%d/external=%t", kind, useOAuth, status, external), func(t *testing.T) {
						ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
						defer cancel()
						var foreign, observedCredential, observedPost, active atomic.Int32
						failures := make(chan string, 32)
						sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1); w.WriteHeader(http.StatusNoContent) }))
						defer sink.Close()
						// This fixture has a fixed tool set. Do not advertise change
						// subscriptions: the SDK's modern subscriptions/listen method
						// does not terminate with its legacy SSE GET on server shutdown.
						server := sdk.NewServer(&sdk.Implementation{Name: "captured-resource"}, &sdk.ServerOptions{
							Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
						})
						sdk.AddTool(server, &sdk.Tool{Name: "identify"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
							return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "captured-resource"}}}, nil, nil
						})
						var handler http.Handler
						if kind == "http" {
							handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
						} else {
							handler = sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return server }, nil)
						}
						var endpointURL string
						endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							active.Add(1)
							defer active.Add(-1)
							correct := r.Header.Get("X-Credential") == "captured-resource-secret"
							if useOAuth {
								correct = r.Header.Get("Authorization") == "Bearer captured-oauth-secret"
							}
							if correct {
								observedCredential.Add(1)
							} else {
								select {
								case failures <- "resource request lost captured credential":
								default:
								}
							}
							if r.Method == http.MethodPost {
								body, err := io.ReadAll(r.Body)
								if err != nil || !bytes.Contains(body, []byte(`"jsonrpc"`)) {
									select {
									case failures <- "resource POST lost MCP body":
									default:
									}
								}
								r.Body = io.NopCloser(bytes.NewReader(body))
								observedPost.Add(1)
							}
							if r.URL.Path == "/start" {
								destination := endpointURL + "/mcp"
								if external {
									destination = sink.URL + "/mcp"
								}
								http.Redirect(w, r, destination, status)
								return
							}
							handler.ServeHTTP(w, r)
						}))
						defer endpoint.Close()
						endpointURL = endpoint.URL
						trustMCPResourceServers(t, sink, endpoint)
						selected := config.MCPConfig{Type: config.MCPHttp, URL: endpoint.URL + "/start", Timeout: 3, Headers: map[string]string{"X-Credential": "captured-resource-secret"}}
						if kind == "sse" {
							selected.Type = config.MCPSSE
						}
						if useOAuth {
							selected.OAuth = true
							selected.Headers = nil
							selected.OAuthToken = &oauth.Token{AccessToken: "captured-oauth-secret", RefreshToken: "captured-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), Client: &oauth.OAuthClient{ClientID: "captured-client", TokenURL: endpoint.URL + "/token", AuthStyle: int(oauth2.AuthStyleInParams)}}
						}
						store := config.NewTestStore(&config.Config{MCP: config.MCPs{"fixture": selected}})
						manager := For(store)
						defer closeMCPDestinationManager(t, manager)
						err := manager.InitializeSingle(ctx, "fixture", store)
						if external {
							require.ErrorContains(t, err, "provider redirect refused")
						} else {
							require.NoError(t, err)
							result, err := manager.RunTool(ctx, store, "fixture", "identify", "{}")
							require.NoError(t, err)
							require.Equal(t, "captured-resource", result.Content)
							require.Positive(t, observedPost.Load())
						}
						require.Positive(t, observedCredential.Load())
						require.Zero(t, foreign.Load(), "refused origin must receive no resource request")
						select {
						case failure := <-failures:
							t.Fatal(failure)
						default:
						}
						closeMCPDestinationManager(t, manager)
						require.Eventually(t, func() bool { return active.Load() == 0 }, 3*time.Second, time.Millisecond, "manager close must drain the real HTTP/SSE requests")
					})
				}
			}
		}
	}
}

func TestMCPSSEAdvertisedPostCannotMoveCapturedCredentialsHTTPS(t *testing.T) {
	for _, useOAuth := range []bool{false, true} {
		t.Run(fmt.Sprintf("oauth=%t", useOAuth), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var foreign, observed atomic.Int32
			sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1); w.WriteHeader(http.StatusNoContent) }))
			defer sink.Close()
			endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				correct := r.Header.Get("X-Credential") == "captured-resource-secret"
				if useOAuth {
					correct = r.Header.Get("Authorization") == "Bearer captured-oauth-secret"
				}
				if correct {
					observed.Add(1)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s/post\n\n", sink.URL)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer endpoint.Close()
			trustMCPResourceServers(t, sink, endpoint)
			selected := config.MCPConfig{Type: config.MCPSSE, URL: endpoint.URL + "/events", Timeout: 3, Headers: map[string]string{"X-Credential": "captured-resource-secret"}}
			if useOAuth {
				selected.OAuth = true
				selected.Headers = nil
				selected.OAuthToken = &oauth.Token{AccessToken: "captured-oauth-secret", ExpiresAt: time.Now().Add(time.Hour).Unix(), Client: &oauth.OAuthClient{ClientID: "captured-client", TokenURL: endpoint.URL + "/token", AuthStyle: int(oauth2.AuthStyleInParams)}}
			}
			store := config.NewTestStore(&config.Config{MCP: config.MCPs{"fixture": selected}})
			manager := For(store)
			defer closeMCPDestinationManager(t, manager)
			err := manager.InitializeSingle(ctx, "fixture", store)
			require.ErrorContains(t, err, "provider redirect refused")
			require.Positive(t, observed.Load(), "initial allowed SSE request must carry the selected credential")
			require.Zero(t, foreign.Load(), "advertised endpoint must be checked before header/token injection")
		})
	}
}
