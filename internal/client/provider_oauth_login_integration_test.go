package client

import (
	"context"
	"encoding/json"
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

	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/callbackrelay"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func newOAuthRPCStore(t *testing.T, host *httptest.Server, mode string) (string, *config.ConfigStore, string, string) {
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
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, values[key])
	}
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"]))
	require.NoError(t, err)
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: bundle, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
	require.NoError(t, err)
	manager.Close()
	require.NoError(t, os.MkdirAll(values["CRUX_GLOBAL_CONFIG"], 0700))
	path := filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json")
	document := `{"providers":{"example-responses":{"plugin":{"id":"example.responses-oauth"},"api_key":"synthetic-old","configuration":{"oauth_client_id":"synthetic-client"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner"},"small":{"provider":"example-responses","model":"example-small"}}}`
	require.NoError(t, os.WriteFile(path, []byte(document), 0600))
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

func awaitOAuthRPCPhase(t *testing.T, sdk *Client, ref providerauth.OAuthLoginRef, phase providerauth.OAuthLoginPhase) providerauth.OAuthLoginState {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var after uint64
	for {
		response, err := sdk.WaitProviderOAuthLogin(ctx, ref.Target.WorkspaceID, ref, after)
		require.NoError(t, err)
		require.NotNil(t, response.State)
		if response.State.Phase == phase {
			return *response.State
		}
		require.NotEqual(t, providerauth.OAuthLoginComplete, response.State.Phase)
		after = response.State.Sequence
	}
}

func TestProviderOAuthRegisteredRoutesManifestRelayAndLostReplies(t *testing.T) {
	for _, mode := range []string{"loopback-dynamic", "hosted-paste", "device-code", "lost replies"} {
		t.Run(mode, func(t *testing.T) {
			flowMode := mode
			if mode == "lost replies" {
				flowMode = "loopback-dynamic"
			}
			var tokenCalls, publications atomic.Int32
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/device":
					_, _ = io.WriteString(w, `{"device_code":"synthetic-private-device","user_code":"VISIBLE-CODE","verification_uri":"https://auth.example/device","expires_in":120,"interval":1}`)
				case "/token":
					tokenCalls.Add(1)
					require.NoError(t, r.ParseForm())
					require.Equal(t, "synthetic-client", r.Form.Get("client_id"))
					if flowMode == "device-code" {
						require.Equal(t, "synthetic-private-device", r.Form.Get("device_code"))
					} else {
						require.Equal(t, "synthetic-code", r.Form.Get("code"))
						require.NotEmpty(t, r.Form.Get("code_verifier"))
					}
					_, _ = io.WriteString(w, `{"access_token":"synthetic-new-$LITERAL","refresh_token":"synthetic-refresh","expires_in":120}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer provider.Close()
			root, store, path, namespace := newOAuthRPCStore(t, provider, flowMode)
			models := store.Config().Models
			store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
				return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() { publications.Add(1); require.Same(t, snapshot.Config(), store.Config()) }}, nil
			})
			appCtx, cancelApp := context.WithCancel(t.Context())
			a := app.NewForTest(appCtx)
			t.Cleanup(func() { cancelApp(); a.ShutdownForTest() })
			ownerServer := server.NewServer(store, "unix", "")
			ws := &backend.Workspace{ID: "oauth-workspace", Path: root, App: a, Cfg: store}
			backend.InsertWorkspaceForTest(ownerServer.Backend(), ws)
			backend.SetWorkspaceShutdownFnForTest(ws, func() {})
			t.Cleanup(ownerServer.Backend().Shutdown)
			var lostBegin, lostCode, lostComplete atomic.Bool
			handler := ownerServer.Handler()
			rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var dropped *atomic.Bool
				if mode == "lost replies" {
					switch r.URL.Path {
					case "/v1/workspaces/oauth-workspace/auth/oauth/begin":
						dropped = &lostBegin
					case "/v1/workspaces/oauth-workspace/auth/oauth/code":
						dropped = &lostCode
					case "/v1/workspaces/oauth-workspace/auth/oauth/complete":
						dropped = &lostComplete
					}
				}
				if dropped != nil && dropped.CompareAndSwap(false, true) {
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, r)
					require.Equal(t, 200, response.Code, "drop only a successful actual owner reply")
					connection, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					require.NoError(t, connection.Close())
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer rpc.Close()
			sdk := captureClient(t, rpc)
			status, err := sdk.ProviderAuthentication(t.Context(), ws.ID)
			require.NoError(t, err)
			owner, ok := store.RuntimeSnapshot().ProviderOwner("example-responses")
			require.True(t, ok)
			ref := providerauth.OAuthLoginRef{LoginID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32), Target: providerauth.Target{WorkspaceID: ws.ID, Owner: providerauth.PublicOwner(owner), Generation: status.Generation}}
			beginCtx, cancelBegin := context.WithCancel(t.Context())
			begun, err := sdk.BeginProviderOAuthLogin(beginCtx, ws.ID, ref)
			if mode == "lost replies" {
				require.Error(t, err)
				begun, err = sdk.BeginProviderOAuthLogin(beginCtx, ws.ID, ref)
			}
			require.NoError(t, err)
			require.NotNil(t, begun.State)
			cancelBegin()
			var submitted providerauth.OAuthLoginCodeRequest
			if flowMode != "device-code" {
				var state providerauth.OAuthLoginState
				var relay *callbackrelay.Relay
				if flowMode == "loopback-dynamic" {
					state = awaitOAuthRPCPhase(t, sdk, ref, providerauth.OAuthLoginWaitingLoopback)
					descriptor := oauth.CallbackRequirement{Mode: state.Callback.Mode, Port: state.Callback.Port, Path: state.Callback.Path}
					relay, err = callbackrelay.Start(t.Context(), descriptor)
					require.NoError(t, err)
					defer relay.Close()
					binding := providerauth.OAuthLoginBindRequest{Login: ref, BindingID: strings.Repeat("c", 32), Port: relay.Port()}
					bound, err := sdk.BindProviderOAuthLogin(t.Context(), ws.ID, binding)
					require.NoError(t, err)
					require.Equal(t, binding.BindingID, bound.BindingID)
					require.Equal(t, binding.Port, bound.Port)
					state = awaitOAuthRPCPhase(t, sdk, ref, providerauth.OAuthLoginWaitingBrowser)
					_, err = sdk.BindProviderOAuthLogin(t.Context(), ws.ID, binding)
					require.NoError(t, err)
				} else {
					state = awaitOAuthRPCPhase(t, sdk, ref, providerauth.OAuthLoginWaitingCode)
				}
				authorization, err := url.Parse(state.AuthorizationURL)
				require.NoError(t, err)
				input := url.Values{"code": {"synthetic-code"}, "state": {authorization.Query().Get("state")}}.Encode()
				if relay != nil {
					callback, err := url.Parse(authorization.Query().Get("redirect_uri"))
					require.NoError(t, err)
					callback.RawQuery = input
					response, err := (&http.Client{Timeout: 2 * time.Second}).Get(callback.String())
					require.NoError(t, err)
					message, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					require.Equal(t, 200, response.StatusCode)
					require.Contains(t, string(message), "received")
					require.NotContains(t, string(message), "saved")
					input, err = relay.Wait(t.Context())
					require.NoError(t, err)
				}
				submitted = providerauth.OAuthLoginCodeRequest{Login: ref, SubmissionID: strings.Repeat("d", 32), Input: input}
				_, err = sdk.SubmitProviderOAuthLoginCode(t.Context(), ws.ID, submitted)
				if mode == "lost replies" {
					require.Error(t, err)
					_, err = sdk.SubmitProviderOAuthLoginCode(t.Context(), ws.ID, submitted)
				}
				require.NoError(t, err)
			}
			awaitOAuthRPCPhase(t, sdk, ref, providerauth.OAuthLoginAuthorized)
			completed, err := sdk.CompleteProviderOAuthLogin(t.Context(), ws.ID, ref)
			if mode == "lost replies" {
				require.Error(t, err)
				completed, err = sdk.CompleteProviderOAuthLogin(t.Context(), ws.ID, ref)
			}
			require.NoError(t, err)
			require.NotNil(t, completed.Workspace)
			require.NoError(t, completed.ValidateOAuthLogin(ref))
			require.True(t, completed.Outcome.Progress.AccountsSaved && completed.Outcome.Progress.ConfigSaved && completed.Outcome.Progress.RuntimePublished)
			require.Equal(t, models, store.Config().Models)
			current, _ := store.Config().Providers.Get(owner.ProviderID)
			require.Equal(t, "synthetic-new-$LITERAL", current.APIKey)
			account, err := accounts.Active(t.Context(), namespace)
			require.NoError(t, err)
			require.Equal(t, current.APIKey, account.AccessToken)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			replay, err := sdk.CompleteProviderOAuthLogin(t.Context(), ws.ID, ref)
			require.NoError(t, err)
			require.Equal(t, completed, replay)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, data, after)
			if flowMode != "device-code" {
				result, err := sdk.SubmitProviderOAuthLoginCode(t.Context(), ws.ID, submitted)
				require.NoError(t, err)
				require.Equal(t, providerauth.OAuthLoginComplete, result.State.Phase)
				submitted.Input += "different"
				_, err = sdk.SubmitProviderOAuthLoginCode(t.Context(), ws.ID, submitted)
				require.ErrorIs(t, err, providerauth.ErrOperationConflict)
			}
			encoded, err := json.Marshal(completed)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "synthetic-new-$LITERAL")
			require.NotContains(t, string(encoded), "synthetic-refresh")
			require.NotContains(t, string(encoded), "account_namespace")
			require.EqualValues(t, 1, tokenCalls.Load())
			require.EqualValues(t, 1, publications.Load())
			ended, err := sdk.CancelProviderOAuthLogin(t.Context(), ws.ID, ref)
			require.NoError(t, err)
			require.Equal(t, providerauth.OAuthLoginComplete, ended.State.Phase)
			status, err = sdk.ProviderAuthentication(t.Context(), ws.ID)
			require.NoError(t, err)
			ref.LoginID, ref.OperationID, ref.Target.Generation = strings.Repeat("e", 32), strings.Repeat("f", 32), status.Generation
			_, err = sdk.BeginProviderOAuthLogin(t.Context(), ws.ID, ref)
			require.NoError(t, err)
			canceled, err := sdk.CancelProviderOAuthLogin(t.Context(), ws.ID, ref)
			require.ErrorIs(t, err, providerauth.ErrOAuthLogin)
			require.Equal(t, providerauth.OAuthLoginCanceled, canceled.State.Phase)
			canceled, err = sdk.WaitProviderOAuthLogin(t.Context(), ws.ID, ref, canceled.State.Sequence)
			require.ErrorIs(t, err, providerauth.ErrOAuthLogin)
			require.Equal(t, providerauth.OAuthLoginCanceled, canceled.State.Phase)
			require.EqualValues(t, 1, tokenCalls.Load())
		})
	}
}

// The positive TLS path creates its workspace through the registered API so the
// backend captures the certificate principal itself. No authority hook is used.
func TestProviderOAuthRegisteredRoutesTLSOwnerIsolation(t *testing.T) {
	var tokenCalls atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		tokenCalls.Add(1)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "synthetic-client", r.Form.Get("client_id"))
		require.Equal(t, "synthetic-tls-code", r.Form.Get("code"))
		require.NotEmpty(t, r.Form.Get("code_verifier"))
		_, _ = io.WriteString(w, `{"access_token":"synthetic-tls-access","refresh_token":"synthetic-tls-refresh","expires_in":120}`)
	}))
	defer provider.Close()
	root, initial, _, namespace := newOAuthRPCStore(t, provider, "hosted-paste")
	for _, entry := range initial.Environment() {
		key, value, found := strings.Cut(entry, "=")
		if found {
			t.Setenv(key, value)
		}
	}
	for _, key := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	saved := make([]connection.Connection, 2)
	for i, name := range []string{"oauth-owner", "other-approved-client"} {
		var code string
		saved[i], code, err = connection.Add(t.Context(), name, "tcp://127.0.0.1:9443", serverCode)
		require.NoError(t, err)
		require.NoError(t, connection.AuthorizeClient(t.Context(), name, code))
	}
	ownerServer := server.NewServer(initial, "tcp", "127.0.0.1:0")
	ownerServer.Backend().SetCreateGrace(time.Minute)
	require.NoError(t, ownerServer.SetWorkspaceRoots([]string{root}))
	require.NoError(t, ownerServer.EnableNetworkAuth(t.Context()))
	t.Cleanup(ownerServer.Backend().Shutdown)
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	rpc := httptest.NewUnstartedServer(ownerServer.Handler())
	rpc.TLS = tlsConfig
	rpc.StartTLS()
	t.Cleanup(rpc.Close)
	clients := make([]*Client, 2)
	for i := range saved {
		saved[i].Address = "tcp://" + rpc.Listener.Addr().String()
		clients[i], err = NewAuthenticatedClient(root, saved[i])
		require.NoError(t, err)
		t.Cleanup(clients[i].h.CloseIdleConnections)
	}
	sdk := clients[0]
	created, err := sdk.CreateWorkspace(t.Context(), proto.Workspace{Path: root, AuthorityMode: "server"})
	require.NoError(t, err)
	ws, err := ownerServer.Backend().GetWorkspace(created.ID)
	require.NoError(t, err)
	t.Cleanup(ws.Shutdown)
	models := ws.Cfg.Config().Models
	status, err := sdk.ProviderAuthentication(t.Context(), ws.ID)
	require.NoError(t, err)
	owner, found := ws.Cfg.RuntimeSnapshot().ProviderOwner("example-responses")
	require.True(t, found)
	ref := providerauth.OAuthLoginRef{LoginID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32), Target: providerauth.Target{WorkspaceID: ws.ID, Owner: providerauth.PublicOwner(owner), Generation: status.Generation}}
	_, err = sdk.BeginProviderOAuthLogin(t.Context(), ws.ID, ref)
	require.NoError(t, err)
	state := awaitOAuthRPCPhase(t, sdk, ref, providerauth.OAuthLoginWaitingCode)
	for _, action := range oauthSDKActions(ref) {
		t.Run("other-certificate/"+action.name, func(t *testing.T) {
			result, err := action.call(t.Context(), clients[1], ws.ID)
			require.Error(t, err)
			require.Zero(t, result)
		})
	}
	authorization, err := url.Parse(state.AuthorizationURL)
	require.NoError(t, err)
	input := url.Values{"code": {"synthetic-tls-code"}, "state": {authorization.Query().Get("state")}}.Encode()
	_, err = sdk.SubmitProviderOAuthLoginCode(t.Context(), ws.ID, providerauth.OAuthLoginCodeRequest{Login: ref, SubmissionID: strings.Repeat("c", 32), Input: input})
	require.NoError(t, err)
	awaitOAuthRPCPhase(t, sdk, ref, providerauth.OAuthLoginAuthorized)
	completed, err := sdk.CompleteProviderOAuthLogin(t.Context(), ws.ID, ref)
	require.NoError(t, err)
	require.NoError(t, completed.ValidateOAuthLogin(ref))
	require.True(t, completed.Outcome.Progress.AccountsSaved)
	require.True(t, completed.Outcome.Progress.ConfigSaved)
	require.True(t, completed.Outcome.Progress.RuntimePublished)
	require.NotNil(t, completed.Workspace)
	require.Equal(t, models, ws.Cfg.Config().Models)
	account, err := accounts.Active(t.Context(), namespace)
	require.NoError(t, err)
	require.Equal(t, "synthetic-tls-access", account.AccessToken)
	require.EqualValues(t, 1, tokenCalls.Load())
	repeated, err := sdk.CompleteProviderOAuthLogin(t.Context(), ws.ID, ref)
	require.NoError(t, err)
	require.Equal(t, completed, repeated)
}
