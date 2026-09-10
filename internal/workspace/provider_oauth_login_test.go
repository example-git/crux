package workspace

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/callbackrelay"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

type workspaceOAuthFixture struct {
	w                     *ClientWorkspace
	transport             *clientAuthenticationFixture
	store                 *config.ConfigStore
	ref                   providerauth.OAuthLoginRef
	root, path, namespace string
}

func awaitWorkspaceOAuthPhase(t *testing.T, f workspaceOAuthFixture, phase providerauth.OAuthLoginPhase) providerauth.OAuthLoginState {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var sequence uint64
	for {
		state, err := f.w.WaitProviderOAuthLogin(ctx, f.ref, sequence)
		require.NoError(t, err)
		require.NoError(t, state.Validate())
		if state.Phase == phase {
			return state
		}
		require.NotContains(t, []providerauth.OAuthLoginPhase{providerauth.OAuthLoginCanceled, providerauth.OAuthLoginExpired, providerauth.OAuthLoginFailed, providerauth.OAuthLoginComplete}, state.Phase)
		sequence = state.Sequence
	}
}

func authorizeWorkspaceOAuth(t *testing.T, f workspaceOAuthFixture, mode string) providerauth.OAuthLoginCodeRequest {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	_, err := f.w.BeginProviderOAuthLogin(ctx, f.ref)
	require.NoError(t, err)
	cancel() // The Begin reply context does not own the admitted session.
	if mode == "device-code" {
		awaitWorkspaceOAuthPhase(t, f, providerauth.OAuthLoginAuthorized)
		return providerauth.OAuthLoginCodeRequest{}
	}
	early, err := f.w.CompleteProviderOAuthLogin(t.Context(), f.ref)
	require.Error(t, err)
	require.Equal(t, providerauth.MutationProgress{}, early.Progress)
	require.NotContains(t, f.w.authority.authenticationReceipts, f.ref.OperationID)
	var state providerauth.OAuthLoginState
	var relay *callbackrelay.Relay
	if mode == "loopback-dynamic" {
		state = awaitWorkspaceOAuthPhase(t, f, providerauth.OAuthLoginWaitingLoopback)
		require.NotNil(t, state.Callback)
		relay, err = callbackrelay.Start(t.Context(), oauth.CallbackRequirement{Mode: state.Callback.Mode, Port: state.Callback.Port, Path: state.Callback.Path})
		require.NoError(t, err)
		defer relay.Close()
		binding := providerauth.OAuthLoginBindRequest{Login: f.ref, BindingID: strings.Repeat("c", 32), Port: relay.Port()}
		_, err = f.w.BindProviderOAuthLogin(t.Context(), binding)
		require.NoError(t, err)
		state = awaitWorkspaceOAuthPhase(t, f, providerauth.OAuthLoginWaitingBrowser)
		again, err := f.w.BindProviderOAuthLogin(t.Context(), binding)
		require.NoError(t, err)
		require.Equal(t, state, again)
		binding.Port++
		_, err = f.w.BindProviderOAuthLogin(t.Context(), binding)
		require.ErrorIs(t, err, providerauth.ErrOperationConflict)
	} else {
		state = awaitWorkspaceOAuthPhase(t, f, providerauth.OAuthLoginWaitingCode)
	}
	uri, err := url.Parse(state.AuthorizationURL)
	require.NoError(t, err)
	input := url.Values{"code": {"workspace-code"}, "state": {uri.Query().Get("state")}}.Encode()
	if relay != nil {
		callback, err := url.Parse(uri.Query().Get("redirect_uri"))
		require.NoError(t, err)
		require.Equal(t, fmt.Sprint(relay.Port()), callback.Port())
		callback.RawQuery = input
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, callback.String(), nil)
		require.NoError(t, err)
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.NoError(t, response.Body.Close())
		input, err = relay.Wait(t.Context())
		require.NoError(t, err)
	}
	submission := providerauth.OAuthLoginCodeRequest{Login: f.ref, SubmissionID: strings.Repeat("d", 32), Input: input}
	_, err = f.w.SubmitProviderOAuthLoginCode(t.Context(), submission)
	require.NoError(t, err)
	awaitWorkspaceOAuthPhase(t, f, providerauth.OAuthLoginAuthorized)
	_, err = f.w.SubmitProviderOAuthLoginCode(t.Context(), submission)
	require.NoError(t, err)
	return submission
}

func TestWorkspaceOAuthTLSAcknowledgedCompletionAndRecovery(t *testing.T) {
	for _, test := range []struct{ flow, disposition string }{{"loopback-dynamic", "acknowledged"}, {"hosted-paste", "lost-response"}, {"device-code", "rejected"}, {"hosted-paste", "review"}} {
		t.Run(test.flow+"/"+test.disposition, func(t *testing.T) {
			var exchanges, inferences atomic.Int32
			literal := "workspace-$(no-command)-$LITERAL"
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/device":
					_, _ = fmt.Fprint(w, `{"device_code":"device-private","user_code":"VISIBLE","verification_uri":"https://auth.example/device","expires_in":120,"interval":1}`)
				case "/token":
					exchanges.Add(1)
					require.NoError(t, r.ParseForm())
					require.Equal(t, "synthetic-client", r.Form.Get("client_id"))
					if test.flow == "device-code" {
						require.Equal(t, "device-private", r.Form.Get("device_code"))
					} else {
						require.Equal(t, "workspace-code", r.Form.Get("code"))
						require.NotEmpty(t, r.Form.Get("code_verifier"))
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": literal, "refresh_token": "workspace-refresh", "expires_in": 3600})
				case "/v1/responses":
					inferences.Add(1)
					require.Equal(t, "Bearer "+literal, r.Header.Get("Authorization"))
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, `{"id":"oauth-json","status":"completed","output":[{"id":"message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"oauth accepted","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
				default:
					t.Errorf("unexpected provider path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer host.Close()
			f := newWorkspaceOAuthFixture(t, host, test.flow)
			serverFiles := []string{f.transport.path, f.transport.accountsPath}
			infos, files := clientAuthenticationFiles(t, serverFiles...)
			models := f.store.RuntimeSnapshot().AgentModelState()
			submission := authorizeWorkspaceOAuth(t, f, test.flow)
			baseline := f.transport.puts.Load()
			switch test.disposition {
			case "lost-response":
				f.transport.putMode.Store(2)
				f.transport.getMode.Store(1)
			case "rejected", "review":
				f.transport.putMode.Store(1)
			}
			outcome, err := f.w.CompleteProviderOAuthLogin(t.Context(), f.ref)
			if test.disposition == "acknowledged" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, outcome.ValidateOAuthLogin(f.ref))
			require.NotNil(t, outcome.Change)
			require.Equal(t, providerauth.MutationProgress{AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}, outcome.Progress)
			require.EqualValues(t, baseline+1, f.transport.puts.Load())
			localInfos, localFiles := clientAuthenticationFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
			f.transport.putMode.Store(0)
			f.transport.getMode.Store(0)
			replay, err := f.w.CompleteProviderOAuthLogin(t.Context(), f.ref)
			require.Equal(t, outcome, replay)
			require.EqualValues(t, baseline+1, f.transport.puts.Load())
			switch test.disposition {
			case "review":
				require.Error(t, err)
				require.NoError(t, f.store.SetConfigField(config.ScopeGlobal, "options.notifications", "disabled"))
				// This separate normal reload resolves model defaults. Review
				// publishes its current saved model state, not the older login
				// receipt's model snapshot.
				models = f.store.RuntimeSnapshot().AgentModelState()
				original := f.w.authority.authenticationReceipts[f.ref.OperationID].request
				review := ProviderAuthenticationReviewRequest{OperationID: f.ref.OperationID, OriginalTarget: f.ref.Target, ReviewID: strings.Repeat("e", 32), ReviewSequence: 1}
				summary, err := f.w.ReviewProviderAuthentication(t.Context(), review)
				require.NoError(t, err)
				require.Equal(t, f.ref.LoginID, summary.OriginalLoginID)
				require.Equal(t, outcome.Change.Current.Status.ActiveAccountID, summary.OriginalAccountID)
				require.Equal(t, original, f.w.authority.authenticationReceipts[f.ref.OperationID].request)
				apply := ProviderAuthenticationApplyRequest{OperationID: f.ref.OperationID, OriginalTarget: f.ref.Target, ReviewID: review.ReviewID, PreviewID: summary.PreviewID, ApplyID: strings.Repeat("f", 32)}
				applied, err := f.w.ApplyProviderAuthenticationReview(t.Context(), apply)
				require.NoError(t, err)
				require.True(t, applied.RemoteAcknowledged && applied.Adopted)
				repeated, err := f.w.ApplyProviderAuthenticationReview(t.Context(), apply)
				require.NoError(t, err)
				require.Equal(t, applied, repeated)
				require.EqualValues(t, baseline+2, f.transport.puts.Load())
				localInfos, localFiles = clientAuthenticationFiles(t, f.path, filepath.Join(f.root, "accounts", "accounts.json"))
			case "rejected":
				require.Error(t, err)
				recovery := ProviderAuthenticationRecoveryRequest{OperationID: f.ref.OperationID, Target: f.ref.Target, RecoveryID: strings.Repeat("e", 32), RecoverySequence: 1}
				recovered, err := f.w.RecoverProviderAuthentication(t.Context(), recovery)
				require.NoError(t, err)
				require.Equal(t, outcome, recovered)
				recovered, err = f.w.RecoverProviderAuthentication(t.Context(), recovery)
				require.NoError(t, err)
				require.NoError(t, recovered.ValidateOAuthLogin(f.ref))
				require.EqualValues(t, baseline+2, f.transport.puts.Load())
			default:
				require.NoError(t, err)
			}
			if submission.SubmissionID != "" {
				state, err := f.w.SubmitProviderOAuthLoginCode(t.Context(), submission)
				require.NoError(t, err)
				require.Equal(t, providerauth.OAuthLoginComplete, state.Phase)
			}
			state, err := f.w.BeginProviderOAuthLogin(t.Context(), f.ref)
			require.NoError(t, err)
			require.Equal(t, providerauth.OAuthLoginComplete, state.Phase)
			require.Equal(t, models, f.store.RuntimeSnapshot().AgentModelState())
			require.True(t, f.store.Config().CanInitializeAgent())
			require.NoError(t, f.w.InitCoderAgentNonInteractive(t.Context()))
			receiver, err := f.transport.s.Backend().GetWorkspace(f.w.workspaceID())
			require.NoError(t, err)
			require.Equal(t, models, receiver.Cfg.RuntimeSnapshot().AgentModelState())
			result, err := receiver.CurrentAgentCoordinator().Model().Model.Generate(t.Context(), fantasy.Call{Headers: map[string]string{"x-session-id": "oauth-acceptance"}, Prompt: fantasy.Prompt{fantasy.NewUserMessage("OAuth accepted credential")}})
			require.NoError(t, err)
			require.Equal(t, "oauth accepted", result.Content[0].(fantasy.TextContent).Text)
			require.EqualValues(t, 1, inferences.Load())
			require.EqualValues(t, 1, exchanges.Load())
			account, err := accounts.Active(t.Context(), f.namespace)
			require.NoError(t, err)
			require.Equal(t, literal, account.AccessToken)
			requireClientAuthenticationFilesUnchanged(t, serverFiles, infos, files)
			requireClientAuthenticationFilesUnchanged(t, []string{f.path, filepath.Join(f.root, "accounts", "accounts.json")}, localInfos, localFiles)
		})
	}
}

func TestWorkspaceOAuthCanceledWaitDoesNotCancelLogin(t *testing.T) {
	var exchanges atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_, _ = fmt.Fprint(w, `{"access_token":"wait-token","expires_in":3600}`)
	}))
	defer host.Close()
	f := newWorkspaceOAuthFixture(t, host, "hosted-paste")
	_, err := f.w.BeginProviderOAuthLogin(t.Context(), f.ref)
	require.NoError(t, err)
	state := awaitWorkspaceOAuthPhase(t, f, providerauth.OAuthLoginWaitingCode)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := f.w.WaitProviderOAuthLogin(ctx, f.ref, state.Sequence)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled OAuth wait remained blocked")
	}
	still, err := f.w.WaitProviderOAuthLogin(t.Context(), f.ref, 0)
	require.NoError(t, err)
	require.Equal(t, state, still)
	// An uncanceled wait releases the authority lock, permitting explicit
	// cancellation to enter and publish its terminal state.
	go func() {
		_, err := f.w.WaitProviderOAuthLogin(t.Context(), f.ref, state.Sequence)
		done <- err
	}()
	canceled, err := f.w.CancelProviderOAuthLogin(t.Context(), f.ref)
	require.True(t, err == nil || errors.Is(err, context.Canceled))
	require.Equal(t, providerauth.OAuthLoginCanceled, canceled.Phase)
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("explicit cancel could not wake OAuth wait")
	}
	require.Zero(t, exchanges.Load())
}

func TestWorkspaceOAuthCallerCancellationRetainsAdmittedLocalCommit(t *testing.T) {
	var exchanges atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_, _ = fmt.Fprint(w, `{"access_token":"admitted-token","refresh_token":"admitted-refresh","expires_in":3600}`)
	}))
	defer host.Close()
	f := newWorkspaceOAuthFixture(t, host, "hosted-paste")
	authorizeWorkspaceOAuth(t, f, "hosted-paste")
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
	defer cancel()
	type completion struct {
		outcome providerauth.MutationOutcome
		err     error
	}
	done := make(chan completion, 1)
	go func() {
		outcome, err := f.w.CompleteProviderOAuthLogin(ctx, f.ref)
		done <- completion{outcome, err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("local commit never entered preparation")
	}
	cancel()
	close(release)
	var result completion
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("admitted commit did not return its known result")
	}
	require.ErrorIs(t, result.err, context.Canceled)
	require.NoError(t, result.outcome.ValidateOAuthLogin(f.ref))
	require.True(t, result.outcome.Progress.ConfigSaved && result.outcome.Progress.AccountsSaved && result.outcome.Progress.RuntimePublished)
	require.Zero(t, f.transport.puts.Load())
	f.w.authority.mu.Lock()
	receipt := f.w.authority.authenticationReceipts[f.ref.OperationID]
	require.True(t, receipt.after.SameObservation(receipt.after))
	require.Nil(t, receipt.proposal)
	require.True(t, f.w.authority.unacknowledgedClientAuthentication(f.w.workspaceID()))
	f.w.authority.mu.Unlock()
	request := ProviderAuthenticationRecoveryRequest{OperationID: f.ref.OperationID, Target: f.ref.Target, RecoveryID: strings.Repeat("e", 32), RecoverySequence: 1}
	recovered, err := f.w.RecoverProviderAuthentication(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, result.outcome, recovered)
	require.EqualValues(t, 1, f.transport.puts.Load())
	require.EqualValues(t, 1, exchanges.Load())
}

func TestWorkspaceOAuthBeginReplayRejectsCacheChangeDuringServiceWait(t *testing.T) {
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"access_token":"retained-token","refresh_token":"retained-refresh","expires_in":3600}`)
	}))
	defer host.Close()
	f := newWorkspaceOAuthFixture(t, host, "hosted-paste")
	authorizeWorkspaceOAuth(t, f, "hosted-paste")
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
	commit := make(chan error, 1)
	go func() {
		_, err := f.w.authority.providerAuth.CompleteOAuthLogin(t.Context(), f.ref)
		commit <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("commit did not hold the service gate")
	}
	type reply struct {
		state providerauth.OAuthLoginState
		err   error
	}
	done := make(chan reply, 1)
	go func() {
		state, err := f.w.BeginProviderOAuthLogin(t.Context(), f.ref)
		done <- reply{state, err}
	}()
	require.Eventually(t, func() bool {
		if f.w.authority.mu.TryLock() {
			f.w.authority.mu.Unlock()
			return false
		}
		return true
	}, time.Second, time.Millisecond)
	f.w.mu.Lock()
	changed := *f.w.ws.Authority
	changed.Principal = "changed-principal"
	f.w.ws.Authority = &changed
	f.w.mu.Unlock()
	close(release)
	require.NoError(t, <-commit)
	select {
	case result := <-done:
		require.Error(t, result.err)
		require.Empty(t, result.state.Login.LoginID, "a stale cache cannot receive a successful original login state")
	case <-time.After(10 * time.Second):
		t.Fatal("Begin replay stayed blocked")
	}
}

func newWorkspaceOAuthFixture(t *testing.T, host *httptest.Server, mode string, customize ...func(*manifest.Manifest)) workspaceOAuthFixture {
	t.Helper()
	f := newClientAuthenticationFixture(t, false)
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
	for _, edit := range customize {
		edit(&declaration)
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
	document := `{"providers":{"example-responses":{"plugin":{"id":"example.responses-oauth"},"configuration":{"oauth_client_id":"synthetic-client"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner"},"small":{"provider":"example-responses","model":"example-small"}}}`
	require.NoError(t, os.WriteFile(path, []byte(document), 0o600))
	previous := http.DefaultClient
	previousTransport := http.DefaultTransport
	http.DefaultClient = host.Client()
	http.DefaultTransport = host.Client().Transport
	t.Cleanup(func() {
		// Receiver tool construction can still be joining after the client
		// subscription ends. Join the server before restoring shared fixtures.
		for _, remote := range f.s.Backend().ListWorkspaces() {
			if retained, err := f.s.Backend().GetWorkspace(remote.ID); err == nil {
				retained.Shutdown()
			}
		}
		_ = f.s.Close()
		http.DefaultClient = previous
		http.DefaultTransport = previousTransport
	})
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	c := f.w.client
	c.SetLocalRuntimeStore(store)
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), DataDir: t.TempDir(), AuthorityMode: "client", Runtime: &proposal})
	require.NoError(t, err)
	w := NewClientWorkspace(c, *created)
	t.Cleanup(w.Shutdown)
	require.False(t, store.Config().CanInitializeAgent(), "logged-out local configuration must not become executable")
	snapshot, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("example-responses")
	require.True(t, ok)
	ref := providerauth.OAuthLoginRef{LoginID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32), Target: providerauth.Target{WorkspaceID: snapshot.WorkspaceID, Generation: snapshot.Generation, Owner: providerauth.PublicOwner(owner)}}
	return workspaceOAuthFixture{w: w, transport: f, store: store, ref: ref, path: path, namespace: owner.AccountNamespace, root: root}
}
