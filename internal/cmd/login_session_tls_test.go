package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

// Lose the command-facing reply after the actual Workspace transaction. This
// exercises retry identity for local ownership and the mTLS SDK route alike.
type lostCLIReplyWorkspace struct {
	workspace.Workspace
	drop                    bool
	begin, submit, complete int
	ref                     providerauth.OAuthLoginRef
	submission              providerauth.OAuthLoginCodeRequest
}

func (w *lostCLIReplyWorkspace) BeginProviderOAuthLogin(ctx context.Context, r providerauth.OAuthLoginRequest) (providerauth.OAuthLoginState, error) {
	if w.begin > 0 && r != w.ref {
		return providerauth.OAuthLoginState{}, fmt.Errorf("CLI replaced original login")
	}
	w.ref = r
	w.begin++
	state, err := w.Workspace.BeginProviderOAuthLogin(ctx, r)
	if err == nil && w.drop && w.begin == 1 {
		return providerauth.OAuthLoginState{}, io.ErrUnexpectedEOF
	}
	return state, err
}
func (w *lostCLIReplyWorkspace) SubmitProviderOAuthLoginCode(ctx context.Context, r providerauth.OAuthLoginCodeRequest) (providerauth.OAuthLoginState, error) {
	if w.submit > 0 && r != w.submission {
		return providerauth.OAuthLoginState{}, fmt.Errorf("CLI replaced original submission")
	}
	w.submission = r
	w.submit++
	state, err := w.Workspace.SubmitProviderOAuthLoginCode(ctx, r)
	if err == nil && w.drop && w.submit == 1 {
		return providerauth.OAuthLoginState{}, io.ErrUnexpectedEOF
	}
	return state, err
}
func (w *lostCLIReplyWorkspace) CompleteProviderOAuthLogin(ctx context.Context, r providerauth.OAuthLoginRef) (providerauth.MutationOutcome, error) {
	if r != w.ref {
		return providerauth.MutationOutcome{}, fmt.Errorf("CLI replaced original completion")
	}
	w.complete++
	outcome, err := w.Workspace.CompleteProviderOAuthLogin(ctx, r)
	if err == nil && w.drop && w.complete == 1 {
		return providerauth.MutationOutcome{}, io.ErrUnexpectedEOF
	}
	return outcome, err
}
func (w *lostCLIReplyWorkspace) CanRecoverProviderAuthentication() bool {
	capability, ok := w.Workspace.(workspace.ProviderAuthenticationRecoverer)
	return ok && capability.CanRecoverProviderAuthentication()
}
func (w *lostCLIReplyWorkspace) RecoverProviderAuthentication(ctx context.Context, r workspace.ProviderAuthenticationRecoveryRequest) (providerauth.MutationOutcome, error) {
	return w.Workspace.(workspace.ProviderAuthenticationRecoverer).RecoverProviderAuthentication(ctx, r)
}

func TestCLIOAuthSessionThroughTLS(t *testing.T) {
	for _, test := range []struct {
		flow, authority           string
		drop, reject, cancelInput bool
	}{
		{"loopback-dynamic", "client", false, false, false},
		{"hosted-paste", "client", true, false, false},
		{"device-code", "client", false, false, false},
		{"hosted-paste", "server", true, false, false},
		{"hosted-paste", "client", false, true, false},
		{"hosted-paste", "client", false, false, true},
	} {
		t.Run(fmt.Sprintf("%s/%s/drop=%t/recovery=%t/cancel=%t", test.flow, test.authority, test.drop, test.reject, test.cancelInput), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			var exchanges, inferences, puts atomic.Int32
			literal := "cli-$(literal)-$TOKEN"
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/device":
					fmt.Fprint(w, `{"device_code":"private-device","user_code":"VISIBLE","verification_uri":"https://auth.example/device","expires_in":120,"interval":1}`)
				case "/token":
					exchanges.Add(1)
					require.NoError(t, r.ParseForm())
					require.Equal(t, "synthetic-client", r.Form.Get("client_id"))
					if test.flow == "device-code" {
						require.Equal(t, "private-device", r.Form.Get("device_code"))
					} else {
						require.Equal(t, "cli-code", r.Form.Get("code"))
						require.NotEmpty(t, r.Form.Get("code_verifier"))
					}
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"access_token": literal, "refresh_token": "cli-refresh", "expires_in": 3600}))
				case "/v1/responses":
					inferences.Add(1)
					require.Equal(t, "Bearer "+literal, r.Header.Get("Authorization"))
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"id":"cli-json","status":"completed","output":[{"id":"message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"cli accepted","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
				default:
					t.Errorf("unexpected provider path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(provider.Close)
			oldClient, oldTransport := http.DefaultClient, http.DefaultTransport
			http.DefaultClient = provider.Client()
			http.DefaultTransport = provider.Client().Transport
			t.Cleanup(func() { http.DefaultClient = oldClient; http.DefaultTransport = oldTransport })
			store, root, values := newCLIOAuthStore(t, provider, test.flow)
			serverRoot := t.TempDir()
			for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR"} {
				value := filepath.Join(serverRoot, key)
				require.NoError(t, os.MkdirAll(value, 0700))
				t.Setenv(key, value)
			}
			t.Setenv("CRUX_PROVIDER_PROFILE", "integrated")
			t.Setenv("CRUX_PROVIDER_PLUGINS", "")
			t.Setenv("HERDR_PANE_ID", "")
			var initial *config.ConfigStore
			if test.authority == "server" {
				initial = store
				for key, value := range values {
					t.Setenv(key, value)
				}
			}
			serverCode, err := connection.EnsureServerIdentity(ctx)
			require.NoError(t, err)
			identity, err := connection.NewClientIdentity("cli-owner")
			require.NoError(t, err)
			require.NoError(t, connection.AuthorizeClient(ctx, "cli-owner", identity.Certificate))
			tlsConfig, err := connection.ServerTLSConfig(ctx)
			require.NoError(t, err)
			ownerServer := server.NewServer(initial, "tcp", "127.0.0.1:0")
			ownerServer.Backend().SetCreateGrace(time.Minute)
			require.NoError(t, ownerServer.EnableNetworkAuth(ctx))
			handler := ownerServer.Handler()
			rpc := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/runtime") {
					if puts.Add(1) == 1 && test.reject {
						http.Error(w, "fixture rejected publication", http.StatusConflict)
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			rpc.TLS = tlsConfig
			rpc.StartTLS()
			t.Cleanup(func() { rpc.Close(); _ = ownerServer.Close() })
			api, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + rpc.Listener.Addr().String(), ServerCertificate: serverCode, Client: identity})
			require.NoError(t, err)
			requested := proto.Workspace{Path: root, DataDir: t.TempDir(), AuthorityMode: test.authority}
			if test.authority == "client" {
				api.SetLocalRuntimeStore(store)
				proposal, err := store.CollectRemoteRuntime(ctx, 1)
				require.NoError(t, err)
				requested.Runtime = &proposal
			}
			created, err := api.CreateWorkspace(ctx, requested)
			require.NoError(t, err)
			remote, err := ownerServer.Backend().GetWorkspace(created.ID)
			require.NoError(t, err)
			t.Cleanup(remote.Shutdown)
			retained := workspace.NewClientWorkspace(api, *created)
			t.Cleanup(retained.Shutdown)
			wrapped := &lostCLIReplyWorkspace{Workspace: retained, drop: test.drop}
			input, writer, err := os.Pipe()
			require.NoError(t, err)
			t.Cleanup(func() { input.Close(); writer.Close() })
			if test.drop {
				_, err = fmt.Fprint(writer, "r\n")
				require.NoError(t, err)
			}
			var output bytes.Buffer
			opened := 0
			callbackURL := ""
			open := func(raw string) error {
				opened++
				if test.cancelInput {
					time.AfterFunc(30*time.Millisecond, cancel)
					return nil
				}
				uri, err := url.Parse(raw)
				require.NoError(t, err)
				if test.flow == "device-code" {
					return nil
				}
				code := url.Values{"code": {"cli-code"}, "state": {uri.Query().Get("state")}}.Encode()
				if test.flow == "loopback-dynamic" {
					callback, err := url.Parse(uri.Query().Get("redirect_uri"))
					require.NoError(t, err)
					callback.RawQuery = code
					callbackURL = callback.String()
					response, err := (&http.Client{Transport: oldTransport}).Get(callbackURL)
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, response.StatusCode)
					response.Body.Close()
				} else {
					_, err = fmt.Fprintln(writer, code)
					require.NoError(t, err)
				}
				if test.drop {
					_, err = fmt.Fprint(writer, "r\nr\n")
					require.NoError(t, err)
				}
				if test.reject {
					_, err = fmt.Fprint(writer, "p\n")
					require.NoError(t, err)
				}
				return nil
			}
			err = runWorkspaceLogin(ctx, wrapped, []string{"example-responses"}, true, input, &output, open, nil)
			if test.cancelInput {
				require.ErrorIs(t, err, context.Canceled)
				require.NotContains(t, output.String(), "Authenticated with")
				require.Zero(t, exchanges.Load())
				state, _ := retained.WaitProviderOAuthLogin(t.Context(), wrapped.ref, 0)
				require.Equal(t, providerauth.OAuthLoginCanceled, state.Phase)
				return
			}
			require.NoError(t, err, output.String())
			require.Contains(t, output.String(), "Authenticated with")
			require.Equal(t, 1, opened)
			require.EqualValues(t, 1, exchanges.Load())
			if test.drop {
				require.Equal(t, 2, wrapped.begin)
				require.Equal(t, 2, wrapped.submit)
				require.Equal(t, 2, wrapped.complete)
			}
			if test.authority == "client" {
				expected := int32(1)
				if test.reject {
					expected = 2
					require.Contains(t, output.String(), "Remote acknowledgement is not confirmed")
				}
				require.Equal(t, expected, puts.Load())
			} else {
				require.Zero(t, puts.Load())
			}
			beforeBegin := wrapped.begin
			require.NoError(t, runWorkspaceLogin(ctx, wrapped, []string{"example-responses"}, false, strings.NewReader(""), &output, open, nil))
			require.Contains(t, output.String(), "already logged in")
			require.Equal(t, beforeBegin, wrapped.begin)
			require.EqualValues(t, 1, exchanges.Load())
			require.NoError(t, retained.InitCoderAgentNonInteractive(ctx))
			response, err := remote.App.CurrentAgentCoordinator().Model().Model.Generate(ctx, fantasy.Call{Headers: map[string]string{"x-session-id": "cli-oauth-acceptance"}, Prompt: fantasy.Prompt{fantasy.NewUserMessage("Use the CLI's acknowledged credential")}})
			require.NoError(t, err)
			require.Equal(t, "cli accepted", response.Content[0].(fantasy.TextContent).Text)
			require.EqualValues(t, 1, inferences.Load())
			if callbackURL != "" {
				_, err := (&http.Client{Transport: oldTransport, Timeout: time.Second}).Get(callbackURL)
				require.Error(t, err, "command must close its callback listener")
			}
		})
	}
}

func newCLIOAuthStore(t *testing.T, host *httptest.Server, mode string) (*config.ConfigStore, string, map[string]string) {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "oauth.plugin")
	require.NoError(t, os.CopyFS(bundle, os.DirFS("../../docs/provider-plugins/examples/responses-oauth.plugin")))
	data, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	require.NoError(t, err)
	var declaration manifest.Manifest
	require.NoError(t, json.Unmarshal(data, &declaration))
	endpoint, err := url.Parse(host.URL)
	require.NoError(t, err)
	for i := range declaration.Capabilities.Endpoints {
		e := &declaration.Capabilities.Endpoints[i]
		e.BaseURL = host.URL + "/" + e.ID
		e.AllowedHosts = []string{endpoint.Hostname()}
		e.AllowedSchemes = []string{"https"}
	}
	flow := &declaration.Capabilities.OAuth[0]
	flow.Redirect.Mode = mode
	if mode == "hosted-paste" {
		flow.Redirect.URI = "https://callback.example/complete"
	}
	if mode == "device-code" {
		flow.Redirect = manifest.OAuthRedirect{Mode: mode}
		flow.PKCE = "disabled"
		declaration.Capabilities.Endpoints = append(declaration.Capabilities.Endpoints, manifest.Endpoint{ID: "device", BaseURL: host.URL + "/device", AllowedSchemes: []string{"https"}, AllowedHosts: []string{endpoint.Hostname()}, Override: "forbidden"})
		flow.DeviceCode = &manifest.DeviceCodeFlow{Endpoint: "device", Request: []manifest.FieldRule{{Name: "client_id", Value: manifest.Template{Kind: "context", Ref: "oauth.client_id"}}}, DeviceCodePointer: "/device_code", UserCodePointer: "/user_code", VerificationURLPointer: "/verification_uri", ExpiresInPointer: "/expires_in", IntervalPointer: "/interval", DefaultIntervalSeconds: 1, Poll: []manifest.FieldRule{{Name: "device_code", Value: manifest.Template{Kind: "context", Ref: "oauth.device_code"}}}, ErrorPointer: "/error", MaxBodyBytes: 1024}
		flow.DeviceCode.Poll = append(flow.DeviceCode.Poll, manifest.FieldRule{Name: "client_id", Value: manifest.Template{Kind: "context", Ref: "oauth.client_id"}})
	}
	data, err = json.Marshal(declaration)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), data, 0600))
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfilePluginNative), "CRUX_PROVIDER_PLUGINS": "example-responses"}
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"]))
	require.NoError(t, err)
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: bundle, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
	require.NoError(t, err)
	manager.Close()
	require.NoError(t, os.MkdirAll(values["CRUX_GLOBAL_CONFIG"], 0700))
	path := filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json")
	document := `{"providers":{"example-responses":{"plugin":{"id":"example.responses-oauth"},"configuration":{"oauth_client_id":"synthetic-client"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner"},"small":{"provider":"example-responses","model":"example-small"}}}`
	require.NoError(t, os.WriteFile(path, []byte(document), 0600))
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	return store, root, values
}
