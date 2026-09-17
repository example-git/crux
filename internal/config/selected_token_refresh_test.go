package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type selectedTokenFixture struct {
	store      *ConfigStore
	owner      providerregistry.RegistrationOwner
	original   *oauth.Token
	successor  *oauth.Token
	requests   atomic.Int32
	afterRead  func()
	accountDir string
	ambientDir string
}

func newSelectedTokenFixture(t *testing.T, clientPresent bool) *selectedTokenFixture {
	t.Helper()
	root := t.TempDir()
	f := &selectedTokenFixture{accountDir: filepath.Join(root, "captured-accounts"), ambientDir: filepath.Join(root, "ambient-accounts")}
	f.original = &oauth.Token{AccessToken: "original-access", RefreshToken: "original-refresh", ExpiresIn: 777, ExpiresAt: time.Now().Add(-time.Hour).Unix(), Client: &oauth.OAuthClient{
		ClientID: "original-registration", ClientSecret: "original-client-secret", AuthURL: "https://registration.invalid/authorize", TokenURL: "https://registration.invalid/token", AuthStyle: 2,
	}}
	f.successor = &oauth.Token{AccessToken: "successor-access", RefreshToken: "successor-refresh", ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		assert.NotNil(t, r.TLS)
		assert.Equal(t, "/token", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, r.ParseForm())
		assert.Equal(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {f.original.RefreshToken}, "client_id": {"captured-client"}}, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(f.successor))
	}))
	t.Cleanup(server.Close)
	registry, err := providerregistry.New(registrytest.Registrations()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("codex")
	require.True(t, ok)
	registration.AccountNamespace = ""
	registration.OAuth.Refresh = func(ctx context.Context, refresh string) (*oauth.Token, error) {
		clientID, present := oauth.LookupEnvironment(ctx, "TOKEN_REFRESH_CLIENT_ID")
		if !present || clientID == "" {
			return nil, errors.New("captured refresh client is not configured")
		}
		values := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/token", strings.NewReader(values.Encode()))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := providertransport.ClientWithContextOwnerValidator(ctx, server.Client()).Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		var token oauth.Token
		if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
			return nil, err
		}
		if f.afterRead != nil {
			f.afterRead()
		}
		return &token, nil
	}
	provider := ProviderConfig{ID: "codex", Plugin: &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}, Name: "Namespace-free OAuth", Type: catalog.TypeOpenAICompat, BaseURL: "https://inference.invalid/v1", APIKey: f.original.AccessToken, OAuthToken: cloneOAuthToken(f.original), Owner: providerOwnerReferenceForRegistration(registration), Models: []catalog.Model{{ID: "fixture", Name: "Fixture"}}}
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig](), Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: "codex", Model: "fixture"}, SelectedModelTypeSmall: {Provider: "codex", Model: "fixture"}}}
	cfg.Providers.Set("codex", provider)
	f.store = NewTestStoreWithRegistrations(cfg, registration)
	f.store.workingDir, f.store.globalDataPath = root, filepath.Join(root, "config.json")
	values := map[string]string{"AI_CLI_DIR": f.accountDir, "HOME": root, "USERPROFILE": root, "CODEX_CLI_VERSION": "captured-version"}
	if clientPresent {
		values["TOKEN_REFRESH_CLIENT_ID"] = "captured-client"
	}
	f.store.baseEnvironment = env.NewFromMap(values)
	f.store.effectiveEnvironment = cloneEnvironment(f.store.baseEnvironment)
	f.store.resolver = IdentityResolver()
	f.owner, ok = f.store.RuntimeSnapshot().ProviderOwner("codex")
	require.True(t, ok)
	require.Empty(t, f.owner.AccountNamespace)
	document, err := json.Marshal(map[string]any{"providers": map[string]any{"codex": provider}, "foreign": json.RawMessage(`{"decimal":1.0,"large":9007199254740993}`)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.store.globalDataPath, document, 0o600))
	t.Setenv("AI_CLI_DIR", f.ambientDir)
	t.Setenv("TOKEN_REFRESH_CLIENT_ID", "ambient-client")
	return f
}

func (f *selectedTokenFixture) noAccounts(t *testing.T) {
	t.Helper()
	for _, path := range []string{f.accountDir, f.ambientDir} {
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err), "namespace-free refresh created account storage")
	}
}

func TestSelectedTokenRefreshHTTPSPreservesMetadataAndReplaysSavedSuccessor(t *testing.T) {
	for _, returnedClient := range []bool{false, true} {
		t.Run(fmt.Sprint(returnedClient), func(t *testing.T) {
			f := newSelectedTokenFixture(t, true)
			if returnedClient {
				f.successor.Client = &oauth.OAuthClient{ClientID: "refreshed-registration", ClientSecret: "refreshed-client-secret", AuthURL: "https://refreshed.invalid/authorize", TokenURL: "https://refreshed.invalid/token", AuthStyle: 1}
			}
			admitted := f.store.RuntimeSnapshot()
			before, err := os.ReadFile(f.store.globalDataPath)
			require.NoError(t, err)
			fresh, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admitted)
			require.NoError(t, err)
			want := cloneOAuthToken(f.successor)
			if want.Client == nil {
				want.Client = clonePointer(f.original.Client)
			}
			require.Equal(t, want, fresh)
			for _, secret := range []string{f.original.AccessToken, f.original.RefreshToken, f.original.Client.ClientSecret, want.AccessToken, want.RefreshToken, want.Client.ClientSecret} {
				require.NotContains(t, redact.String("credential="+secret), secret)
			}
			require.NotSame(t, f.original.Client, fresh.Client)
			provider, _ := f.store.Config().Providers.Get("codex")
			require.Equal(t, want, provider.OAuthToken)
			require.NotSame(t, fresh.Client, provider.OAuthToken.Client)
			fresh.Client.ClientSecret = "caller mutation"
			fresh.AccessToken = "caller mutation"
			adopted, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, f.store.RuntimeSnapshot())
			require.NoError(t, err)
			require.Equal(t, want, adopted)
			require.EqualValues(t, 1, f.requests.Load())
			after, err := os.ReadFile(f.store.globalDataPath)
			require.NoError(t, err)
			require.Equal(t, gjson.GetBytes(before, "foreign").Raw, gjson.GetBytes(after, "foreign").Raw)
			var saved oauth.Token
			require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(after, "providers.codex.oauth").Raw), &saved))
			require.Equal(t, want, &saved)
			originalProvider, _ := admitted.Config().Providers.Get("codex")
			require.Equal(t, f.original, originalProvider.OAuthToken)
			f.noAccounts(t)
		})
	}
}

func TestSelectedTokenRefreshRejectsChangedAdmissionBeforeHTTPS(t *testing.T) {
	for _, mode := range []string{"memory-token", "disk-client", "duplicate-json", "aliased-root", "aliased-oauth", "captured-environment", "captured-absence", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newSelectedTokenFixture(t, mode != "captured-absence")
			admitted := f.store.RuntimeSnapshot()
			ctx := t.Context()
			switch mode {
			case "memory-token":
				f.store.mutateInMemory(func(cfg *Config) {
					provider, _ := cfg.Providers.Get("codex")
					provider.OAuthToken = cloneOAuthToken(provider.OAuthToken)
					provider.OAuthToken.RefreshToken = "manual-refresh"
					cfg.Providers.Set("codex", provider)
				})
			case "disk-client":
				data, err := os.ReadFile(f.store.globalDataPath)
				require.NoError(t, err)
				data, err = sjson.SetBytes(data, "providers.codex.oauth.client.client_id", "manual-registration")
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.store.globalDataPath, data, 0o600))
			case "duplicate-json":
				data, err := os.ReadFile(f.store.globalDataPath)
				require.NoError(t, err)
				data = []byte(strings.Replace(string(data), `"foreign":`, `"duplicate":1,"duplicate":2,"foreign":`, 1))
				require.NoError(t, os.WriteFile(f.store.globalDataPath, data, 0o600))
			case "aliased-root", "aliased-oauth":
				data, err := os.ReadFile(f.store.globalDataPath)
				require.NoError(t, err)
				path, value := "providers.codex.OAuth", any(f.original)
				if mode == "aliased-root" {
					path, value = "Providers", json.RawMessage(gjson.GetBytes(data, "providers").Raw)
				}
				data, err = sjson.SetBytes(data, path, value)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.store.globalDataPath, data, 0o600))
			case "captured-environment":
				f.store.effectiveEnvironment = env.NewFromMap(map[string]string{"TOKEN_REFRESH_CLIENT_ID": "different"})
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before, err := os.ReadFile(f.store.globalDataPath)
			require.NoError(t, err)
			fresh, err := f.store.RefreshProviderOAuthTokenForRuntime(ctx, ScopeGlobal, f.owner, f.original, admitted)
			require.Error(t, err)
			require.Nil(t, fresh)
			require.Zero(t, f.requests.Load())
			after, err := os.ReadFile(f.store.globalDataPath)
			require.NoError(t, err)
			require.Equal(t, before, after)
			f.noAccounts(t)
		})
	}
}

func TestSelectedTokenRefreshRetainsRotationAcrossConfigWriteFailure(t *testing.T) {
	f := newSelectedTokenFixture(t, true)
	admitted := f.store.RuntimeSnapshot()
	before, err := os.ReadFile(f.store.globalDataPath)
	require.NoError(t, err)
	f.afterRead = func() { require.NoError(t, os.Mkdir(f.store.globalDataPath+".lock", 0o700)) }
	fresh, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admitted)
	require.ErrorContains(t, err, "rotated and retained")
	require.Equal(t, f.successor.AccessToken, fresh.AccessToken)
	require.Same(t, admitted.Config(), f.store.Config())
	after, err := os.ReadFile(f.store.globalDataPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoError(t, os.Remove(f.store.globalDataPath+".lock"))
	f.afterRead = nil
	fresh, err = f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admitted)
	require.NoError(t, err)
	require.Equal(t, f.successor.AccessToken, fresh.AccessToken)
	require.EqualValues(t, 1, f.requests.Load())
	f.noAccounts(t)
}

func TestSelectedTokenRefreshCompletesAfterDisconnectAndSharesRotation(t *testing.T) {
	f := newSelectedTokenFixture(t, true)
	admitted := f.store.RuntimeSnapshot()
	ctx, cancel := context.WithCancel(t.Context())
	f.afterRead = cancel
	fresh, err := f.store.RefreshProviderOAuthTokenForRuntime(ctx, ScopeGlobal, f.owner, f.original, admitted)
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	f.afterRead = nil
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			got, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admitted)
			assert.NoError(t, err)
			assert.Equal(t, fresh, got)
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, f.requests.Load())
	f.noAccounts(t)
}

func TestSelectedTokenRefreshDefinitionChangeRetainsSuccessorWithoutReexchange(t *testing.T) {
	f := newSelectedTokenFixture(t, true)
	admitted := f.store.RuntimeSnapshot()
	f.afterRead = func() {
		f.store.mutateInMemory(func(cfg *Config) {
			provider, _ := cfg.Providers.Get("codex")
			provider.ExtraHeaders = map[string]string{"X-Changed": "definition"}
			cfg.Providers.Set("codex", provider)
		})
	}
	fresh, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admitted)
	require.Error(t, err)
	require.NotNil(t, fresh)
	f.afterRead = nil
	_, err = f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, f.store.RuntimeSnapshot())
	require.ErrorContains(t, err, "retained rotation")
	require.EqualValues(t, 1, f.requests.Load())
	f.noAccounts(t)
}

func TestSelectedTokenRefreshCacheBoundRejectsBeforeExchange(t *testing.T) {
	f := newSelectedTokenFixture(t, true)
	f.store.selectedTokenRotations = make(map[string]*selectedTokenRotation)
	for i := range selectedTokenRotationLimit {
		f.store.selectedTokenRotations[fmt.Sprint(i)] = &selectedTokenRotation{freshMu: new(sync.RWMutex), fresh: &oauth.Token{AccessToken: "pending"}}
	}
	_, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, f.store.RuntimeSnapshot())
	require.ErrorContains(t, err, "too many retained")
	require.Zero(t, f.requests.Load())
	require.Len(t, f.store.selectedTokenRotations, selectedTokenRotationLimit)
	f.noAccounts(t)
}

func TestSelectedTokenRefreshLoadedLoginPreservesAuthenticationBasis(t *testing.T) {
	for _, changedSource := range []string{"unchanged", "before-exchange", "after-exchange"} {
		t.Run(changedSource, func(t *testing.T) {
			testSelectedTokenRefreshLoadedLogin(t, changedSource)
		})
	}
}

func testSelectedTokenRefreshLoadedLogin(t *testing.T, changedSource string) {
	f := newAuthenticationCandidateFixture(t, "codex", false, false, "")
	original := oauthLoginToken()
	exchange, calls := authenticationCOWHTTPS(t, original.RefreshToken)
	owner := oauthLoginRegistration(t, f.store, "codex", func(registration *providerregistry.Registration) {
		registration.AccountNamespace = ""
		registration.OAuth.Authorize = func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
			return cloneOAuthToken(original), nil
		}
		registration.OAuth.Refresh = func(ctx context.Context, refresh string) (*oauth.Token, error) {
			fresh, err := exchange(ctx, refresh)
			if err == nil && changedSource == "after-exchange" {
				authenticationBasisWriteField(t, filepath.Join(f.root, "crux.json"), []string{"providers", "unrelated", "api_key"}, `"different-source-credential"`)
			}
			return fresh, err
		}
	})
	before, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	preparation, err := f.store.PrepareOAuthLogin(t.Context(), before, owner)
	require.NoError(t, err)
	authorized, err := f.store.AuthorizeOAuthLogin(t.Context(), preparation, nil, nil)
	require.NoError(t, err)
	login, err := f.store.CommitOAuthLogin(t.Context(), ScopeGlobal, authorized)
	require.NoError(t, err)
	require.True(t, login.ConfigSaved && login.RuntimePublished)
	require.False(t, login.AccountsSaved)
	admitted := f.store.RuntimeSnapshot()
	originalBasis := admitted.Config().authenticationBasis
	originalAuthority := admitted.Config().authenticationAccounts
	require.NotNil(t, originalBasis)
	if changedSource == "before-exchange" {
		authenticationBasisWriteField(t, filepath.Join(f.root, "crux.json"), []string{"providers", "unrelated", "api_key"}, `"different-source-credential"`)
		unchanged := reconciliationUnchangedFiles(t, f.store.globalDataPath, filepath.Join(f.root, "crux.json"))
		fresh, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, owner, original, admitted)
		require.Error(t, err)
		require.Nil(t, fresh)
		require.Zero(t, calls.Load())
		require.Same(t, admitted.Config(), f.store.Config())
		unchanged()
		return
	}
	credentialBefore, err := os.ReadFile(f.store.globalDataPath)
	require.NoError(t, err)
	fresh, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, owner, original, admitted)
	if changedSource == "after-exchange" {
		require.ErrorContains(t, err, "rotated and retained")
		require.NotNil(t, fresh)
		require.EqualValues(t, 1, calls.Load())
		require.Same(t, admitted.Config(), f.store.Config())
		credentialAfter, err := os.ReadFile(f.store.globalDataPath)
		require.NoError(t, err)
		require.Equal(t, credentialBefore, credentialAfter)
		_, err = f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, owner, original, admitted)
		require.ErrorContains(t, err, "rotated and retained")
		require.EqualValues(t, 1, calls.Load())
		return
	}
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
	require.Equal(t, original.Client, fresh.Client)
	provider, present := f.store.Config().Providers.Get(owner.ProviderID)
	require.True(t, present)
	require.Equal(t, fresh, provider.OAuthToken)
	require.Equal(t, fresh.AccessToken, provider.APIKey)
	require.NotSame(t, originalBasis, f.store.Config().authenticationBasis)
	require.Same(t, originalAuthority, f.store.Config().authenticationAccounts)
	oldProvider, _ := admitted.Config().Providers.Get(owner.ProviderID)
	require.Equal(t, original, oldProvider.OAuthToken)
	after, layers := authenticationBasisCapture(t, f.store, f.store.baseEnvironment)
	require.NoError(t, after.validateConfigBasis(layers, ""))
	require.True(t, before.accounts.SameObservation(after.accounts))
	replay, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, owner, original, admitted)
	require.NoError(t, err)
	require.Equal(t, fresh, replay)
	require.EqualValues(t, 1, calls.Load())
}

func TestSelectedTokenRefreshConcurrentFirstExchange(t *testing.T) {
	f := newSelectedTokenFixture(t, true)
	admitted := f.store.RuntimeSnapshot()
	started, release := make(chan struct{}), make(chan struct{})
	f.afterRead = func() {
		close(started)
		<-release
	}
	want := cloneOAuthToken(f.successor)
	want.Client = clonePointer(f.original.Client)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			got, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admitted)
			assert.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("refresh exchange did not start")
	}
	close(release)
	wg.Wait()
	require.EqualValues(t, 1, f.requests.Load())
	f.noAccounts(t)
}

func TestSelectedTokenRefreshRetainsSuccessorWhileWriterExceedsCompletionDeadline(t *testing.T) {
	// Shrink the real completion deadline so this test observes the exact
	// same production DeadlineExceeded path without waiting out a full
	// minute of real time on every run.
	originalTimeout := selectedTokenCompletionTimeout
	selectedTokenCompletionTimeout = 50 * time.Millisecond
	t.Cleanup(func() { selectedTokenCompletionTimeout = originalTimeout })
	f := newSelectedTokenFixture(t, true)
	admitted := f.store.RuntimeSnapshot()
	held := make(chan struct{})
	f.afterRead = func() {
		f.store.writeMu.Lock()
		close(held)
	}
	type result struct {
		token *oauth.Token
		err   error
	}
	done := make(chan result, 1)
	go func() {
		token, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admitted)
		done <- result{token, err}
	}()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("refresh response was not decoded")
	}
	locked := true
	defer func() {
		if locked {
			f.store.writeMu.Unlock()
		}
	}()
	// The test holds writeMu. Retention must happen independently, before the
	// bounded commit wait, so cancellation cannot orphan the consumed token.
	require.Len(t, f.store.selectedTokenRotations, 1)
	var receipt *selectedTokenRotation
	for _, candidate := range f.store.selectedTokenRotations {
		receipt = candidate
	}
	require.Eventually(t, func() bool { return receipt.token() != nil }, 2*time.Second, 5*time.Millisecond)
	select {
	case got := <-done:
		require.ErrorIs(t, got.err, context.DeadlineExceeded)
		require.ErrorContains(t, got.err, "rotated and retained")
		require.Equal(t, f.successor.AccessToken, got.token.AccessToken)
	case <-time.After(5 * time.Second):
		t.Fatal("global writer exceeded the bounded OAuth completion deadline")
	}
	require.Same(t, admitted.Config(), f.store.Config())
	f.store.writeMu.Unlock()
	locked = false
	f.afterRead = nil
	fresh, err := f.store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admitted)
	require.NoError(t, err)
	require.Equal(t, f.successor.AccessToken, fresh.AccessToken)
	require.EqualValues(t, 1, f.requests.Load())
	f.noAccounts(t)
}
