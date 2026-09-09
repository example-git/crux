package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
)

func loadMCPDestinationAuthenticationStore(t *testing.T, selected config.MCPConfig) (*config.ConfigStore, string) {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{
		"HOME": root, "USERPROFILE": root, "PATH": "/usr/bin:/bin",
		"AI_CLI_DIR":                     filepath.Join(root, "ai-cli"),
		"CRUX_GLOBAL_CONFIG":             filepath.Join(root, "config"),
		"CRUX_GLOBAL_DATA":               filepath.Join(root, "data"),
		"CRUX_CACHE_DIR":                 filepath.Join(root, "cache"),
		"XDG_CONFIG_HOME":                filepath.Join(root, "xdg-config"),
		"XDG_DATA_HOME":                  filepath.Join(root, "xdg-data"),
		"XDG_CACHE_HOME":                 filepath.Join(root, "xdg-cache"),
		"XDG_STATE_HOME":                 filepath.Join(root, "xdg-state"),
		"CRUX_DISABLE_DEFAULT_PROVIDERS": "true",
		"CRUX_PROVIDER_PROFILE":          "core-only",
	}
	working, workspaceData := filepath.Join(root, "project"), filepath.Join(root, "workspace-data")
	for _, directory := range []string{values["CRUX_GLOBAL_CONFIG"], values["CRUX_GLOBAL_DATA"], working, workspaceData} {
		require.NoError(t, os.MkdirAll(directory, 0o700))
	}
	path := filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json")
	encoded, err := json.Marshal(map[string]any{"mcp": config.MCPs{"fixture": selected}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	store, err := config.LoadIsolated(working, workspaceData, false, env.NewFromMap(values))
	require.NoError(t, err)
	require.NotNil(t, store.Config().MCP["fixture"].OAuthToken)
	return store, path
}

func TestMCPDestinationRefusalDoesNotResetOAuthHTTPS(t *testing.T) {
	for _, renewal := range []bool{false, true} {
		for _, test := range []struct {
			name    string
			status  int
			expired bool
		}{
			{name: "invalid_grant", status: http.StatusTemporaryRedirect},
			{name: "invalid_client", status: http.StatusPermanentRedirect},
			{name: "destination-refused", status: http.StatusTemporaryRedirect},
			{name: "expired-token", expired: true},
		} {
			t.Run(fmt.Sprintf("renewal=%t/%s", renewal, test.name), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				var deny atomic.Bool
				deny.Store(!renewal)
				var foreign, resourceRequests, deniedRequests, refreshRequests, unauthorizedRequests, active atomic.Int32
				failures := make(chan string, 32)
				sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					foreign.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}))
				defer sink.Close()
				server := sdk.NewServer(&sdk.Implementation{Name: "captured-resource"}, &sdk.ServerOptions{
					Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
				})
				sdk.AddTool(server, &sdk.Tool{Name: "identify"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
					return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "captured-resource"}}}, nil, nil
				})
				handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
				endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					active.Add(1)
					defer active.Add(-1)
					if r.URL.Path == "/token" {
						refreshRequests.Add(1)
						_ = r.ParseForm()
						if !test.expired || r.Method != http.MethodPost || r.PostForm.Get("refresh_token") != "captured-refresh" || r.PostForm.Get("client_secret") != "captured-secret" {
							failures <- "unexpected token request or lost captured refresh credentials"
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
						return
					}
					resourceRequests.Add(1)
					if r.Header.Get("Authorization") != "Bearer captured-access" {
						if test.expired && deny.Load() && refreshRequests.Load() > 0 {
							// The SDK deliberately sends without a bearer after
							// invalid_grant, then uses the resource's 401 to
							// report that interactive authentication is required.
							unauthorizedRequests.Add(1)
							w.Header().Set("WWW-Authenticate", `Bearer realm="fixture"`)
							w.WriteHeader(http.StatusUnauthorized)
							return
						}
						failures <- "resource request lost its selected bearer credential"
					}
					if deny.Load() {
						deniedRequests.Add(1)
						if test.expired {
							// Fail the old session's ping. Its replacement must then
							// use the now-expired saved token and actual token endpoint.
							http.Error(w, "old session is unavailable", http.StatusServiceUnavailable)
						} else {
							http.Redirect(w, r, sink.URL+"/"+test.name, test.status)
						}
						return
					}
					handler.ServeHTTP(w, r)
				}))
				defer endpoint.Close()
				defer func() {
					for session := range server.Sessions() {
						require.NoError(t, session.Close())
					}
				}()
				trustMCPResourceServers(t, endpoint, sink)
				token := &oauth.Token{AccessToken: "captured-access", RefreshToken: "captured-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), Client: &oauth.OAuthClient{
					ClientID: "captured-client", ClientSecret: "captured-secret", TokenURL: endpoint.URL + "/token", AuthStyle: int(oauth2.AuthStyleInParams),
				}}
				if test.expired && !renewal {
					token.ExpiresAt = time.Now().Add(-time.Hour).Unix()
				}
				selected := config.MCPConfig{Type: config.MCPHttp, URL: endpoint.URL + "/mcp", OAuth: true, OAuthToken: token, Timeout: 3}
				store, path := loadMCPDestinationAuthenticationStore(t, selected)
				manager := For(store)
				defer closeMCPDestinationManager(t, manager)
				if renewal {
					require.NoError(t, manager.InitializeSingle(ctx, "fixture", store))
					result, err := manager.RunTool(ctx, store, "fixture", "identify", "{}")
					require.NoError(t, err)
					require.Equal(t, "captured-resource", result.Content)
					require.Positive(t, resourceRequests.Load())
					if test.expired {
						expired := *token
						expired.ExpiresAt = time.Now().Add(-time.Hour).Unix()
						require.NoError(t, store.SetConfigField(config.ScopeGlobal, "mcp.fixture.oauth_token", &expired))
					}
					deny.Store(true)
				}
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				beforeToken, err := json.Marshal(store.Config().MCP["fixture"].OAuthToken)
				require.NoError(t, err)
				if renewal {
					_, err = manager.RunTool(ctx, store, "fixture", "identify", "{}")
				} else {
					err = manager.InitializeSingle(ctx, "fixture", store)
				}
				state, ok := manager.GetState("fixture")
				require.True(t, ok)
				after, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				require.Zero(t, foreign.Load())
				if test.expired {
					if renewal {
						require.Error(t, err)
						require.True(t, isOAuthInitErr(err))
						require.False(t, nonRetryableMCPHTTPError(err))
					} else {
						require.NoError(t, err, "startup retains its existing needs-auth result")
					}
					require.Equal(t, StateNeedsAuth, state.State)
					require.Positive(t, refreshRequests.Load(), "must observe an actual OAuth invalid_grant response")
					require.Positive(t, unauthorizedRequests.Load(), "resource must reject the unauthenticated fallback")
					require.Nil(t, store.Config().MCP["fixture"].OAuthToken)
					require.False(t, gjson.GetBytes(after, "mcp.fixture.oauth_token").Exists())
				} else {
					require.ErrorContains(t, err, "provider redirect refused")
					require.True(t, nonRetryableMCPHTTPError(err))
					require.Equal(t, StateError, state.State)
					require.True(t, nonRetryableMCPHTTPError(state.Error))
					require.Positive(t, deniedRequests.Load())
					require.Zero(t, refreshRequests.Load())
					require.Equal(t, before, after, "destination refusal must not rewrite saved credentials")
					afterToken, marshalErr := json.Marshal(store.Config().MCP["fixture"].OAuthToken)
					require.NoError(t, marshalErr)
					require.JSONEq(t, string(beforeToken), string(afterToken))
				}
				closeMCPDestinationManager(t, manager)
				require.Eventually(t, func() bool { return active.Load() == 0 }, 3*time.Second, time.Millisecond)
				select {
				case failure := <-failures:
					t.Fatal(failure)
				default:
				}
			})
		}
	}
}
