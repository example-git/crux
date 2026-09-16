package model

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func newOAuthUIStore(t *testing.T, host *httptest.Server, mode string) (string, *config.ConfigStore, string, string) {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "oauth.plugin")
	require.NoError(t, os.CopyFS(bundle, os.DirFS("../../../docs/provider-plugins/examples/responses-oauth.plugin")))
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
	if mode == "loopback-dynamic" || mode == "loopback-fixed" {
		flow.Redirect.CallbackPath = "/ui/oauth/callback"
	}
	if mode == "loopback-fixed" {
		available, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		flow.Redirect.Port = available.Addr().(*net.TCPAddr).Port
		require.NoError(t, available.Close())
	}
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
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), data, 0o600))
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfilePluginNative), "CRUX_PROVIDER_PLUGINS": "example-responses"}
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, values[key])
	}
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"]))
	require.NoError(t, err)
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: bundle, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
	require.NoError(t, err)
	manager.Close()
	require.NoError(t, os.MkdirAll(values["CRUX_GLOBAL_CONFIG"], 0o700))
	path := filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json")
	document := `{"providers":{"example-responses":{"plugin":{"id":"example.responses-oauth"},"api_key":"","configuration":{"oauth_client_id":"synthetic-client"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner"},"small":{"provider":"example-responses","model":"example-small"}}}`
	require.NoError(t, os.WriteFile(path, []byte(document), 0o600))
	previous := http.DefaultClient
	previousTransport := http.DefaultTransport
	http.DefaultClient = host.Client()
	http.DefaultTransport = host.Client().Transport
	t.Cleanup(func() { http.DefaultClient = previous; http.DefaultTransport = previousTransport })
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("example-responses")
	require.True(t, ok)
	return root, store, path, owner.AccountNamespace
}

type oauthSDKUIWorkspace struct {
	*workspace.ClientWorkspace
	updates         atomic.Int32
	initializations atomic.Int32
}

func (w *oauthSDKUIWorkspace) AgentIsReady() bool                               { return false }
func (w *oauthSDKUIWorkspace) PermissionSkipRequests() bool                     { return false }
func (w *oauthSDKUIWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo { return nil }
func (w *oauthSDKUIWorkspace) UpdateAgentModel(ctx context.Context, state config.AgentModelState) error {
	err := w.ClientWorkspace.UpdateAgentModel(ctx, state)
	if err == nil {
		w.updates.Add(1)
	}
	return err
}

func (w *oauthSDKUIWorkspace) InitCoderAgent(ctx context.Context) error {
	err := w.ClientWorkspace.InitCoderAgent(ctx)
	if err == nil {
		w.initializations.Add(1)
	}
	return err
}

type oauthUIPasteRequest struct{ input string }

// This fixture executes UI commands concurrently, like Bubble Tea: a blocking
// owner Wait cannot prevent the local callback command from delivering input.
func TestOAuthUIThroughWorkspaceTLSAndActualCallback(t *testing.T) {
	for _, ownership := range []string{"server", "client"} {
		for _, flow := range []string{"loopback-dynamic", "loopback-fixed", "hosted-paste", "device-code"} {
			t.Run(ownership+"/"+flow, func(t *testing.T) {
				var tokens, inferences atomic.Int32
				provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/device":
						_, _ = io.WriteString(w, `{"device_code":"synthetic-private-device","user_code":"VISIBLE-CODE","verification_uri":"https://auth.example/device","expires_in":120,"interval":1}`)
					case r.URL.Path == "/token":
						tokens.Add(1)
						require.NoError(t, r.ParseForm())
						require.Equal(t, "synthetic-client", r.Form.Get("client_id"))
						if flow == "device-code" {
							require.Equal(t, "synthetic-private-device", r.Form.Get("device_code"))
						} else {
							require.Equal(t, "synthetic-ui-code", r.Form.Get("code"))
							require.NotEmpty(t, r.Form.Get("code_verifier"))
						}
						_, _ = io.WriteString(w, `{"access_token":"synthetic-ui-$LITERAL","refresh_token":"synthetic-ui-refresh","expires_in":3600}`)
					case strings.HasSuffix(r.URL.Path, "/responses"):
						inferences.Add(1)
						require.Equal(t, "Bearer synthetic-ui-$LITERAL", r.Header.Get("Authorization"))
						_, _ = io.Copy(io.Discard, r.Body)
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"oauth-ui-json","status":"completed","output":[{"id":"message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"oauth UI accepted","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
					default:
						http.NotFound(w, r)
					}
				}))
				defer provider.Close()
				root, store, _, namespace := newOAuthUIStore(t, provider, flow)
				for _, entry := range store.Environment() {
					key, value, ok := strings.Cut(entry, "=")
					if ok {
						t.Setenv(key, value)
					}
				}
				for _, key := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
					t.Setenv(key, filepath.Join(root, key))
				}
				serverCode, err := connection.EnsureServerIdentity(t.Context())
				require.NoError(t, err)
				saved, clientCode, err := connection.Add(t.Context(), "oauth-ui", "tcp://127.0.0.1:9443", serverCode)
				require.NoError(t, err)
				require.NoError(t, connection.AuthorizeClient(t.Context(), "oauth-ui", clientCode))
				ownerServer := server.NewServer(store, "tcp", "127.0.0.1:0")
				ownerServer.Backend().SetCreateGrace(time.Minute)
				require.NoError(t, ownerServer.SetWorkspaceRoots([]string{root}))
				require.NoError(t, ownerServer.EnableNetworkAuth(t.Context()))
				t.Cleanup(ownerServer.Backend().Shutdown)
				tlsConfig, err := connection.ServerTLSConfig(t.Context())
				require.NoError(t, err)
				backend := httptest.NewUnstartedServer(ownerServer.Handler())
				backend.TLS = tlsConfig
				backend.StartTLS()
				t.Cleanup(backend.Close)
				proxyTLS, err := connection.ClientTLSConfig(saved)
				require.NoError(t, err)
				// Lose successful responses after the real registered handler has
				// admitted the action. The UI must retry the exact original action.
				var lostReplies atomic.Int32
				type replayedRequest struct {
					body  []byte
					count int
				}
				replays := make(map[string]replayedRequest)
				var replayMu sync.Mutex
				var runtimeCommands sync.Map
				rpc := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if ownership == "client" && flow == "hosted-paste" && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/channel") {
						proxyModelWorkspaceChannel(t, w, r, backend.URL, proxyTLS, func(fromClient bool, frame proto.WorkspaceChannelFrame) modelWorkspaceChannelProxyDecision {
							if fromClient && frame.Type == proto.WorkspaceChannelRuntimeReplaceFrame {
								runtimeCommands.Store(frame.CommandID, struct{}{})
							}
							if !fromClient && frame.Type == proto.WorkspaceChannelAcknowledgementFrame {
								if _, ok := runtimeCommands.LoadAndDelete(frame.CommandID); ok && lostReplies.CompareAndSwap(0, 1) {
									return modelWorkspaceChannelProxyDecision{drop: true, close: true}
								}
							}
							return modelWorkspaceChannelProxyDecision{}
						})
						return
					}
					drop := false
					if ownership == "server" && flow == "loopback-dynamic" {
						for _, suffix := range []string{"/auth/oauth/begin", "/auth/oauth/bind", "/auth/oauth/code", "/auth/oauth/complete"} {
							drop = drop || strings.HasSuffix(r.URL.Path, suffix)
						}
					}
					if !drop {
						ownerServer.Handler().ServeHTTP(w, r)
						return
					}
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					r.Body = io.NopCloser(bytes.NewReader(body))
					replayMu.Lock()
					original := replays[r.URL.Path]
					if original.count == 0 {
						original.body = append([]byte(nil), body...)
					}
					if original.count == 1 && r.Method == http.MethodPost {
						require.True(t, bytes.Equal(original.body, body), "lost OAuth reply retry preserves the exact wire request")
					}
					original.count++
					replays[r.URL.Path] = original
					replayMu.Unlock()
					if original.count != 1 {
						ownerServer.Handler().ServeHTTP(w, r)
						return
					}
					recorded := httptest.NewRecorder()
					ownerServer.Handler().ServeHTTP(recorded, r)
					require.GreaterOrEqual(t, recorded.Code, 200)
					require.Less(t, recorded.Code, 300)
					lostReplies.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
				}))
				rpc.TLS = tlsConfig
				rpc.StartTLS()
				t.Cleanup(rpc.Close)
				saved.Address = "tcp://" + rpc.Listener.Addr().String()
				sdk, err := client.NewAuthenticatedClient(root, saved)
				require.NoError(t, err)
				args := proto.Workspace{Path: root, AuthorityMode: ownership}
				if ownership == "client" {
					proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
					require.NoError(t, err)
					args.Runtime = &proposal
					sdk.SetLocalRuntimeStore(store)
				}
				created, err := sdk.CreateWorkspace(t.Context(), args)
				require.NoError(t, err)
				receiver, err := ownerServer.Backend().GetWorkspace(created.ID)
				require.NoError(t, err)
				t.Cleanup(receiver.Shutdown)
				retained := workspace.NewClientWorkspace(sdk, *created)
				t.Cleanup(retained.Shutdown)
				ws := &oauthSDKUIWorkspace{ClientWorkspace: retained}
				ui := newTestUI()
				ui.com.Workspace = ws
				ui = New(ui.com, "", false, "")
				ui.focus = uiFocusNone
				ui.agentBusyCache.set(false)
				ui.yoloCache.set(false)
				ui.lspCheckedAt = time.Now()
				owner, ok := ws.Config().ProviderOwner("example-responses")
				require.True(t, ok)
				selection := dialog.ActionSelectModel{Provider: (&config.ProviderConfig{ID: "example-responses", Name: "Example OAuth"}).ToProvider(), Model: config.SelectedModel{Provider: "example-responses", Model: "example-small"}, ModelType: config.SelectedModelTypeLarge, ProviderOwner: owner, ProviderOwnerSet: true, ReAuthenticate: true}
				messages := make(chan tea.Msg, 128)
				ctx, cancel := context.WithCancel(t.Context())
				var workers sync.WaitGroup
				var dispatch func(tea.Cmd)
				dispatch = func(cmd tea.Cmd) {
					if cmd == nil || ctx.Err() != nil {
						return
					}
					workers.Go(func() {
						message := cmd()
						if batch, ok := message.(tea.BatchMsg); ok {
							for _, child := range batch {
								dispatch(child)
							}
							return
						}
						if message != nil {
							select {
							case messages <- message:
							case <-ctx.Done():
							}
						}
					})
				}
				t.Cleanup(func() {
					cancel()
					for _, op := range ui.oauthLogins {
						if op.cancel != nil {
							op.cancel()
						}
						if op.relay != nil {
							_ = op.relay.Close()
						}
					}
					retained.Shutdown()
					receiver.Shutdown()
					done := make(chan struct{})
					go func() { workers.Wait(); close(done) }()
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("OAuth UI command workers did not stop")
					}
				})
				var opened atomic.Int32
				ui.oauthOpenURL = func(rawURL string) error {
					opened.Add(1)
					authorization, err := url.Parse(rawURL)
					if err != nil {
						return err
					}
					if flow == "device-code" {
						return nil
					}
					input := url.Values{"code": {"synthetic-ui-code"}, "state": {authorization.Query().Get("state")}}.Encode()
					if flow == "hosted-paste" {
						select {
						case messages <- oauthUIPasteRequest{input}:
						case <-ctx.Done():
						}
						return nil
					}
					callback, err := url.Parse(authorization.Query().Get("redirect_uri"))
					if err != nil {
						return err
					}
					require.Equal(t, "/ui/oauth/callback", callback.EscapedPath())
					callback.RawQuery = input
					request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, callback.String(), nil)
					require.NoError(t, err)
					response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
					if err != nil {
						return err
					}
					defer response.Body.Close()
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.Equal(t, 200, response.StatusCode)
					require.Contains(t, string(body), "received")
					require.NotContains(t, string(body), "saved")
					return err
				}
				dispatch(ui.openAuthenticationDialog(selection))
				deadline := time.NewTimer(35 * time.Second)
				defer deadline.Stop()
				retryActions := 0
				newLoginChoices := 0
				for {
					op := ui.oauthLogins[ws]
					if op != nil && op.resolved && len(ui.modelSelectionLanes) == 0 {
						break
					}
					select {
					case message := <-messages:
						if paste, ok := message.(oauthUIPasteRequest); ok {
							d := ui.dialog.Dialog(dialog.LoginID).(*dialog.OAuthLogin)
							_, pasteCmd := ui.Update(tea.PasteMsg{Content: paste.input})
							dispatch(pasteCmd)
							dispatch(ui.handleDialogAction(d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})))
							continue
						}
						_, cmd := ui.Update(message)
						dispatch(cmd)
						if status, ok := message.(apiKeyStatusMsg); ok {
							require.NoError(t, status.err)
							require.NoError(t, status.snapshot.Validate())
							require.Same(t, ws, status.session.workspace)
							require.True(t, status.session.ready)
							require.Equal(t, providerauth.PublicOwner(owner), status.session.target.Owner)
							require.True(t, status.session.target.Owner.HasOAuth)
							require.Empty(t, status.session.slots, "OAuth token compatibility fields are not editable API-key slots")
							require.Empty(t, status.session.credentialID)
							require.False(t, ui.dialog.ContainsDialog(dialog.APIKeyInputID))
							require.IsType(t, &dialog.OAuthLogin{}, ui.dialog.DialogLast())
						}
						if recorded, ok := message.(oauthLoginRecordedMsg); ok {
							require.NoError(t, recorded.err)
							require.NoError(t, recorded.list.Validate())
							require.Equal(t, recorded.target, recorded.list.Target)
							require.Equal(t, created.ID, recorded.target.WorkspaceID)
							require.Equal(t, providerauth.PublicOwner(owner), recorded.target.Owner)
							require.Empty(t, recorded.list.Results)
							require.True(t, recorded.read.recordedLoaded)
							require.Same(t, recorded.read.dialog, ui.dialog.DialogLast())
							require.Nil(t, ui.oauthLogins[ws], "loading recorded results must not start a new exchange")
							require.Zero(t, newLoginChoices)
							// The actual Workspace supports recorded OAuth recovery.
							// Select "Start a new login" after the overlay's 425 ms
							// input grace instead of bypassing that user choice.
							time.Sleep(450 * time.Millisecond)
							_, cmd = ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
							dispatch(cmd)
							newLoginChoices++
							require.NotNil(t, ui.oauthLogins[ws])
							require.Equal(t, recorded.target, ui.oauthLogins[ws].ref.Target)
							require.Empty(t, ui.oauthLogins[ws].recordedOperationID)
						}
						if result, ok := message.(oauthLoginResultMsg); ok && result.err != nil && result.operation != nil && !result.operation.busy && retryActions < int(lostReplies.Load()) {
							action := result.operation.dialog.HandleMsg(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
							require.IsType(t, dialog.ActionOAuthLoginRetry{}, action)
							retryActions++
							dispatch(ui.handleDialogAction(action))
						}
					case <-deadline.C:
						if op != nil {
							t.Fatalf("OAuth UI stopped before acknowledgement: kind=%s phase=%s message=%s", op.kind, op.state.Phase, op.message)
						}
						t.Fatal("OAuth UI did not begin a session")
					}
				}
				op := ui.oauthLogins[ws]
				require.Equal(t, 1, newLoginChoices)
				require.NoError(t, op.outcome.ValidateOAuthLogin(op.ref))
				require.NotNil(t, op.outcome.Change)
				require.EqualValues(t, 1, tokens.Load())
				require.EqualValues(t, 1, opened.Load())
				require.Equal(t, "example-small", ws.Config().Models[config.SelectedModelTypeLarge].Model)
				require.EqualValues(t, 1, ws.initializations.Load(), "real serialized onboarding continuation initializes the agent once")
				require.Zero(t, ws.updates.Load())
				account, err := accounts.Active(t.Context(), namespace)
				require.NoError(t, err)
				require.Equal(t, "synthetic-ui-$LITERAL", account.AccessToken)
				result, err := receiver.CurrentAgentCoordinator().Model().Model.Generate(t.Context(), fantasy.Call{Headers: map[string]string{"x-session-id": "oauth-ui-acceptance"}, Prompt: fantasy.Prompt{fantasy.NewUserMessage("Use the UI-authorized account")}})
				require.NoError(t, err)
				require.Equal(t, "oauth UI accepted", result.Content[0].(fantasy.TextContent).Text)
				require.EqualValues(t, 1, inferences.Load())
				if ownership == "server" && flow == "loopback-dynamic" {
					require.EqualValues(t, 4, lostReplies.Load())
					require.Equal(t, 4, retryActions)
				}
				if ownership == "client" && flow == "hosted-paste" {
					require.EqualValues(t, 1, lostReplies.Load())
					require.LessOrEqual(t, retryActions, 1, "an already accepted publication can be recovered by acknowledgement read")
				}
			})
		}
	}
}
