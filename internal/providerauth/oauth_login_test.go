package providerauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

type oauthServiceFixture struct {
	service   *Service
	store     *config.ConfigStore
	ref       OAuthLoginRef
	path      string
	namespace string
}

func newOAuthServiceFixture(t *testing.T, host *httptest.Server, mode string) oauthServiceFixture {
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
	service := NewWithContext(t.Context(), store, "oauth-fixture")
	t.Cleanup(service.Close)
	snapshot, err := service.Status(t.Context())
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("example-responses")
	require.True(t, ok)
	ref := oauthLoginRefFor(Target{WorkspaceID: snapshot.WorkspaceID, Generation: snapshot.Generation, Owner: PublicOwner(owner)})
	return oauthServiceFixture{service: service, store: store, ref: ref, path: path, namespace: owner.AccountNamespace}
}

func awaitOAuthServicePhase(t *testing.T, f oauthServiceFixture, phase OAuthLoginPhase) OAuthLoginState {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var sequence uint64
	for {
		state, err := f.service.WaitOAuthLogin(ctx, f.ref, sequence)
		require.NoError(t, err)
		if state.Phase == phase {
			return state
		}
		require.False(t, oauthLoginTerminal(state.Phase), "terminal state %s before %s", state.Phase, phase)
		sequence = state.Sequence
	}
}

func submitOAuthServiceCode(t *testing.T, f oauthServiceFixture, mode string) OAuthLoginCodeRequest {
	t.Helper()
	var state OAuthLoginState
	if mode == "loopback-dynamic" {
		awaitOAuthServicePhase(t, f, OAuthLoginWaitingLoopback)
		listener, err := net.Listen("tcp", "localhost:0")
		require.NoError(t, err)
		defer listener.Close()
		port := uint16(listener.Addr().(*net.TCPAddr).Port)
		binding := OAuthLoginBindRequest{Login: f.ref, BindingID: strings.Repeat("c", 32), Port: port}
		_, err = f.service.BindOAuthLogin(t.Context(), binding)
		require.NoError(t, err)
		state = awaitOAuthServicePhase(t, f, OAuthLoginWaitingBrowser)
		uri, err := url.Parse(state.AuthorizationURL)
		require.NoError(t, err)
		require.Contains(t, uri.Query().Get("redirect_uri"), fmt.Sprintf(":%d/callback", port), "owner uses the already occupied client port without binding it")
		replayed, err := f.service.BindOAuthLogin(t.Context(), binding)
		require.NoError(t, err)
		require.Equal(t, state, replayed)
		binding.Port++
		_, err = f.service.BindOAuthLogin(t.Context(), binding)
		require.ErrorIs(t, err, ErrOperationConflict)
	} else {
		state = awaitOAuthServicePhase(t, f, OAuthLoginWaitingCode)
	}
	uri, err := url.Parse(state.AuthorizationURL)
	require.NoError(t, err)
	input := url.Values{"code": {"synthetic-code"}, "state": {uri.Query().Get("state")}}.Encode()
	request := OAuthLoginCodeRequest{Login: f.ref, SubmissionID: strings.Repeat("d", 32), Input: input}
	_, err = f.service.SubmitOAuthLoginCode(t.Context(), request)
	require.NoError(t, err)
	return request
}

func TestOAuthLoginServiceActualManifestExchangeCommitAndReplay(t *testing.T) {
	for _, mode := range []string{"loopback-dynamic", "hosted-paste", "device-code"} {
		t.Run(mode, func(t *testing.T) {
			var tokenCalls, deviceCalls, publications atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/device":
					deviceCalls.Add(1)
					_, _ = w.Write([]byte(`{"device_code":"private-device","user_code":"VISIBLE-CODE","verification_uri":"https://auth.example/device","expires_in":120,"interval":1}`))
				case "/token":
					tokenCalls.Add(1)
					require.NoError(t, r.ParseForm())
					require.Equal(t, "synthetic-client", r.Form.Get("client_id"))
					if mode == "device-code" {
						require.Equal(t, "private-device", r.Form.Get("device_code"))
					} else {
						require.Equal(t, "synthetic-code", r.Form.Get("code"))
						require.NotEmpty(t, r.Form.Get("code_verifier"))
					}
					_, _ = w.Write([]byte(`{"access_token":"synthetic-new-$LITERAL","refresh_token":"synthetic-refresh","expires_in":120}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer host.Close()
			f := newOAuthServiceFixture(t, host, mode)
			models := f.store.Config().Models
			f.store.SetRuntimeGenerationPreparer(func(context.Context, config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
				return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() { publications.Add(1) }}, nil
			})
			beginCtx, cancelBegin := context.WithCancel(t.Context())
			_, err := f.service.BeginOAuthLogin(beginCtx, f.ref)
			require.NoError(t, err)
			cancelBegin()
			var submission OAuthLoginCodeRequest
			if mode != "device-code" {
				submission = submitOAuthServiceCode(t, f, mode)
			}
			awaitOAuthServicePhase(t, f, OAuthLoginAuthorized)
			result, err := f.service.CompleteOAuthLogin(t.Context(), f.ref)
			require.NoError(t, err)
			require.NoError(t, result.Outcome.ValidateOAuthLogin(f.ref))
			require.True(t, result.Outcome.Progress.AccountsSaved && result.Outcome.Progress.ConfigSaved && result.Outcome.Progress.RuntimePublished)
			_, current := result.RuntimeSnapshot()
			require.True(t, current)
			require.Equal(t, models, f.store.Config().Models)
			provider, _ := f.store.Config().Providers.Get("example-responses")
			require.Equal(t, "synthetic-new-$LITERAL", provider.APIKey)
			account, err := accounts.Active(t.Context(), f.namespace)
			require.NoError(t, err)
			require.Equal(t, provider.APIKey, account.AccessToken)
			files := authenticationInputsTree(t, filepath.Dir(filepath.Dir(f.path)))
			replayed, err := f.service.CompleteOAuthLogin(t.Context(), f.ref)
			require.NoError(t, err)
			require.Equal(t, result.Outcome, replayed.Outcome)
			require.Equal(t, files, authenticationInputsTree(t, filepath.Dir(filepath.Dir(f.path))))
			if mode != "device-code" {
				state, err := f.service.SubmitOAuthLoginCode(t.Context(), submission)
				require.NoError(t, err)
				require.Equal(t, OAuthLoginComplete, state.Phase)
				submission.Input += "changed"
				_, err = f.service.SubmitOAuthLoginCode(t.Context(), submission)
				require.ErrorIs(t, err, ErrOperationConflict)
			}
			require.EqualValues(t, 1, tokenCalls.Load())
			require.EqualValues(t, 1, publications.Load())
			if mode == "device-code" {
				require.EqualValues(t, 1, deviceCalls.Load())
			}
		})
	}
}

func TestOAuthLoginServiceLostCommitReplyRetainsExactOwnerAndReceipt(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"synthetic-new","refresh_token":"synthetic-refresh","expires_in":120}`))
	}))
	defer host.Close()
	f := newOAuthServiceFixture(t, host, "loopback-dynamic")
	_, err := f.service.BeginOAuthLogin(t.Context(), f.ref)
	require.NoError(t, err)
	submitOAuthServiceCode(t, f, "loopback-dynamic")
	awaitOAuthServicePhase(t, f, OAuthLoginAuthorized)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.store.SetRuntimeGenerationPreparer(func(context.Context, config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		close(entered)
		<-release
		return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() {}}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	type completion struct {
		result MutationResult
		err    error
	}
	finished := make(chan completion, 1)
	go func() { result, err := f.service.CompleteOAuthLogin(ctx, f.ref); finished <- completion{result, err} }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("commit did not reach preparer")
	}
	cancel()
	response := <-finished
	require.ErrorIs(t, response.err, context.Canceled)
	owner, ok := response.result.OriginalOwner()
	require.True(t, ok)
	require.Equal(t, f.namespace, owner.AccountNamespace)
	close(release)
	result, err := f.service.CompleteOAuthLogin(t.Context(), f.ref)
	require.NoError(t, err)
	require.True(t, result.Outcome.Progress.RuntimePublished)
	require.EqualValues(t, 1, calls.Load())
}
