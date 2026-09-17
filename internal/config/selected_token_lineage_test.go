package config

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type selectedLineageTransport struct {
	origin string
	base   http.RoundTripper
}

func (r selectedLineageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != r.origin {
		return nil, errors.New("lineage fixture blocked nonfixture HTTP")
	}
	return r.base.RoundTrip(request)
}

type installedLineageFixture struct {
	root     string
	values   map[string]string
	owner    providerregistry.RegistrationOwner
	original *oauth.Token
	requests atomic.Int32
	hookMu   sync.Mutex
	hook     func()
}

func newInstalledLineageFixture(t *testing.T) *installedLineageFixture {
	t.Helper()
	f := &installedLineageFixture{root: t.TempDir(), original: &oauth.Token{AccessToken: "installed-old-access", RefreshToken: "installed-old-refresh", ExpiresIn: 1, ExpiresAt: time.Now().Add(-time.Hour).Unix(), Client: &oauth.OAuthClient{ClientID: "retained-client", ClientSecret: "retained-client-secret", TokenURL: "https://registration.invalid/token", AuthURL: "https://registration.invalid/auth", AuthStyle: 2}}}
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		assert.NotNil(t, r.TLS)
		assert.Equal(t, "/token", r.URL.Path)
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		refresh := r.Form.Get("refresh_token")
		assert.Contains(t, []string{f.original.RefreshToken, "installed-fresh-refresh"}, refresh)
		assert.Equal(t, "captured-client", r.Form.Get("client_id"))
		f.hookMu.Lock()
		hook := f.hook
		f.hookMu.Unlock()
		if hook != nil {
			hook()
		}
		w.Header().Set("Content-Type", "application/json")
		if refresh == "installed-fresh-refresh" {
			_, _ = io.WriteString(w, `{"access_token":"installed-next-access","refresh_token":"installed-next-refresh","expires_in":3600}`)
		} else {
			_, _ = io.WriteString(w, `{"access_token":"installed-fresh-access","refresh_token":"installed-fresh-refresh","expires_in":3600}`)
		}
	}))
	t.Cleanup(host.Close)
	parsed, err := url.Parse(host.URL)
	require.NoError(t, err)
	previousClient, previousTransport := http.DefaultClient, http.DefaultTransport
	transport := selectedLineageTransport{origin: parsed.Host, base: host.Client().Transport}
	http.DefaultClient = &http.Client{Transport: transport}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultClient = previousClient; http.DefaultTransport = previousTransport })
	source := filepath.Join(f.root, "lineage.plugin")
	require.NoError(t, os.CopyFS(source, os.DirFS("../../docs/provider-plugins/examples/responses-oauth.plugin")))
	data, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	require.NoError(t, err)
	var declaration manifest.Manifest
	require.NoError(t, json.Unmarshal(data, &declaration))
	declaration.Provider.AccountNamespace = ""
	for index := range declaration.Capabilities.Endpoints {
		endpoint := &declaration.Capabilities.Endpoints[index]
		endpoint.BaseURL = host.URL
		if endpoint.ID == "authorize" {
			endpoint.BaseURL += "/authorize"
		}
		if endpoint.ID == "token" {
			endpoint.BaseURL += "/token"
		}
		endpoint.AllowedHosts = []string{parsed.Hostname()}
	}
	data, err = json.Marshal(declaration)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	f.values = map[string]string{"HOME": f.root, "USERPROFILE": f.root, "AI_CLI_DIR": filepath.Join(f.root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(f.root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(f.root, "data"), "CRUX_CACHE_DIR": filepath.Join(f.root, "cache"), "CRUX_PROVIDER_PROFILE": string(ProviderProfilePluginNative), "CRUX_PROVIDER_PLUGINS": declaration.Provider.ID}
	installTrustedProviderBundle(t, f.values["CRUX_GLOBAL_DATA"], f.values["CRUX_CACHE_DIR"], source)
	data, err = json.Marshal(map[string]any{"providers": map[string]any{declaration.Provider.ID: map[string]any{"plugin": map[string]string{"id": declaration.ID}, "api_key": f.original.AccessToken, "oauth": f.original, "configuration": map[string]string{"oauth_client_id": "captured-client"}}}, "models": map[string]any{"large": map[string]string{"provider": declaration.Provider.ID, "model": "example-reasoner"}, "small": map[string]string{"provider": declaration.Provider.ID, "model": "example-small"}}, "foreign": json.RawMessage(`{"number":1.0,"large":9007199254740993}`)})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(f.values["CRUX_GLOBAL_CONFIG"], 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(f.values["CRUX_GLOBAL_DATA"], "crux.json"), data, 0o600))
	t.Setenv("AI_CLI_DIR", filepath.Join(f.root, "ambient-accounts"))
	t.Setenv("OAUTH_CLIENT_ID", "ambient-client")
	return f
}

func (f *installedLineageFixture) load(t *testing.T) *ConfigStore {
	t.Helper()
	store, err := LoadIsolated(f.root, filepath.Join(f.root, "workspace"), false, env.NewFromMap(f.values))
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("example-responses")
	require.True(t, ok)
	if f.owner.ProviderID == "" {
		f.owner = owner
	} else {
		require.Equal(t, f.owner, owner)
	}
	return store
}

func (f *installedLineageFixture) noAccounts(t *testing.T) {
	t.Helper()
	require.NoDirExists(t, f.values["AI_CLI_DIR"])
	require.NoDirExists(t, filepath.Join(f.root, "ambient-accounts"))
}

func TestSelectedTokenDurableInstalledSuccessorPermissions(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprint(public), func(t *testing.T) {
			f := newInstalledLineageFixture(t)
			store, peer := f.load(t), f.load(t)
			admitted := peer.RuntimeSnapshot()
			fresh, err := store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, store.RuntimeSnapshot())
			require.NoError(t, err)
			input, err := readAuthenticationInput(t.Context(), store.globalDataPath)
			require.NoError(t, err)
			require.True(t, input.privateSuccessor)
			if public {
				makeAuthenticationJournalPublic(t, store.globalDataPath)
			}
			replayed, err := peer.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admitted)
			if public {
				require.ErrorContains(t, err, "OAuth token lineage captured inputs changed")
			} else {
				require.NoError(t, err)
				require.Equal(t, fresh, replayed)
			}
			require.EqualValues(t, 1, f.requests.Load())
		})
	}
}

func TestSelectedTokenDurableInstalledConcurrentStores(t *testing.T) {
	f := newInstalledLineageFixture(t)
	a, b := f.load(t), f.load(t)
	admittedA, admittedB := a.RuntimeSnapshot(), b.RuntimeSnapshot()
	started, release := make(chan struct{}), make(chan struct{})
	f.hookMu.Lock()
	f.hook = func() { close(started); <-release }
	f.hookMu.Unlock()
	type result struct {
		token *oauth.Token
		err   error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go func() {
		token, err := a.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admittedA)
		first <- result{token, err}
	}()
	<-started
	go func() {
		token, err := b.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admittedB)
		second <- result{token, err}
	}()
	close(release)
	gotA, gotB := <-first, <-second
	require.NoError(t, gotA.err)
	require.NoError(t, gotB.err)
	require.Equal(t, gotA.token, gotB.token)
	require.Equal(t, "captured-client", gotA.token.Client.ClientID)
	require.Contains(t, gotA.token.Client.TokenURL, "/token")
	require.Contains(t, gotA.token.Client.AuthURL, "/authorize")
	require.EqualValues(t, 1, f.requests.Load())
	for _, store := range []*ConfigStore{a, b} {
		provider, _ := store.Config().Providers.Get(f.owner.ProviderID)
		require.Equal(t, gotA.token, provider.OAuthToken)
		require.NotNil(t, store.Config().authenticationBasis)
		require.NoError(t, store.validateSelectedTokenSources(t.Context(), store.Config()))
	}
	f.noAccounts(t)
}

func TestSelectedTokenDurableInstalledRestartKnownSuccessor(t *testing.T) {
	for _, mode := range []string{"saved", "returned-before-config"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstalledLineageFixture(t)
			store := f.load(t)
			path := store.globalDataPath
			var release func()
			if mode == "returned-before-config" {
				var err error
				release, err = lock.File(t.Context(), path+".lock")
				require.NoError(t, err)
				defer release()
				// Holding the global config file lock for this subtest
				// forces the commit step's lock.File wait to run out its
				// real deadline (selectedTokenCompletionTimeout, normally
				// a full minute) before the retained-successor error is
				// returned. Shrink it only for this subtest so it
				// observes the exact same production timeout path
				// without waiting out a full minute of real time on
				// every run. The "saved" subtest takes the fast/normal
				// path and must keep the production timeout so it can't
				// spuriously trip a deadline under CI load.
				originalTimeout := selectedTokenCompletionTimeout
				selectedTokenCompletionTimeout = 200 * time.Millisecond
				t.Cleanup(func() { selectedTokenCompletionTimeout = originalTimeout })
			}
			fresh, err := store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, store.RuntimeSnapshot())
			if mode == "returned-before-config" {
				require.ErrorContains(t, err, "rotated and retained")
				require.NotNil(t, fresh)
				release()
			} else {
				require.NoError(t, err)
			}
			require.EqualValues(t, 1, f.requests.Load())
			restarted := f.load(t)
			replayed, err := restarted.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, restarted.RuntimeSnapshot())
			require.NoError(t, err)
			require.Equal(t, fresh, replayed)
			again, err := store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, store.RuntimeSnapshot())
			require.NoError(t, err, "the original store must adopt the peer's recorded completion")
			require.Equal(t, fresh, again)
			require.EqualValues(t, 1, f.requests.Load(), "restart must reuse only the recorded successor")
			provider, _ := restarted.Config().Providers.Get(f.owner.ProviderID)
			require.Equal(t, fresh, provider.OAuthToken)
			require.NotNil(t, restarted.Config().authenticationBasis)
			require.NoError(t, restarted.validateSelectedTokenSources(t.Context(), restarted.Config()))
			file, err := openAuthenticationInput(selectedTokenLineagePath(path, f.owner.ProviderID))
			require.NoError(t, err)
			privacyErr := fsext.ValidatePrivateFile(file)
			require.NoError(t, file.Close())
			require.NoError(t, privacyErr)
			f.noAccounts(t)
		})
	}
}

func TestSelectedTokenDurableMigratesVersionOneLineage(t *testing.T) {
	f := newInstalledLineageFixture(t)
	store := f.load(t)
	admitted := store.RuntimeSnapshot()
	store.writeMu.Lock()
	inputs, err := store.captureAuthenticationInputsLocked(t.Context(), admitted)
	store.writeMu.Unlock()
	require.NoError(t, err)
	fresh, err := store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, admitted)
	require.NoError(t, err)
	path := selectedTokenLineagePath(store.globalDataPath, f.owner.ProviderID)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var journal selectedTokenLineageJournal
	require.NoError(t, json.Unmarshal(data, &journal))
	definition, owner, err := admitted.clientProviderDefinitionRaw(f.owner.ProviderID)
	require.NoError(t, err)
	require.Equal(t, f.owner, owner)
	definitionID, err := definition.Digest()
	require.NoError(t, err)
	identity, err := json.Marshal([]any{ScopeGlobal, f.owner, definitionID, OAuthTokenCredentialID(f.original)})
	require.NoError(t, err)
	legacyKey := stableBytesID(identity)
	legacy := selectedTokenLineageJournal{Version: 1, Sequence: journal.Sequence, Records: map[string]selectedTokenLineageRecord{}}
	for _, record := range journal.Records {
		record.Original = OAuthTokenCredentialID(f.original)
		record.Environment = stableEnvironmentID(admitted.Environment())
		record.Before.Digest = stableBytesID(record.BeforeData)
		for index, proof := range record.Inputs {
			input, found := inputs.file(proof.Path)
			require.True(t, found)
			record.Inputs[index] = selectedTokenLegacyProof(input)
		}
		record.Runtime, err = selectedTokenRuntimeLegacyID(admitted, f.original, record)
		require.NoError(t, err)
		legacy.Records[legacyKey] = record
	}
	data, err = json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	restarted := f.load(t)
	replayed, err := restarted.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, restarted.RuntimeSnapshot())
	require.NoError(t, err)
	require.Equal(t, fresh, replayed)
	require.EqualValues(t, 1, f.requests.Load())
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	journal = selectedTokenLineageJournal{}
	require.NoError(t, json.Unmarshal(data, &journal))
	require.Equal(t, selectedTokenLineageVersion, journal.Version)
	require.NotContains(t, journal.Records, legacyKey)
}

func TestSelectedTokenDurableRestartRejectsUnknownAndDifferentCredentials(t *testing.T) {
	for _, mode := range []string{"unknown-exchange", "different-disk-token", "different-environment", "different-provider"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstalledLineageFixture(t)
			store := f.load(t)
			_, err := store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, store.RuntimeSnapshot())
			require.NoError(t, err)
			path := selectedTokenLineagePath(store.globalDataPath, f.owner.ProviderID)
			if mode == "unknown-exchange" {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				var journal selectedTokenLineageJournal
				require.NoError(t, json.Unmarshal(data, &journal))
				for key, record := range journal.Records {
					record.Successor = nil
					record.Committed = false
					journal.Records[key] = record
				}
				data, err = json.Marshal(journal)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o600))
			}
			if mode == "different-disk-token" {
				authenticationBasisWriteField(t, store.globalDataPath, []string{"providers", f.owner.ProviderID, "oauth", "access_token"}, `"unrelated-newer-token"`)
			}
			if mode == "different-provider" {
				authenticationBasisWriteField(t, store.globalDataPath, []string{"providers", f.owner.ProviderID, "configuration", "oauth_client_id"}, `"other-client"`)
			}
			if mode == "different-environment" {
				f.values["OAUTH_CAPTURED_DIFFERENCE"] = "changed"
			}
			restarted := f.load(t)
			_, err = restarted.RefreshProviderOAuthTokenForRuntime(context.Background(), ScopeGlobal, f.owner, f.original, restarted.RuntimeSnapshot())
			require.Error(t, err)
			require.EqualValues(t, 1, f.requests.Load(), "unknown or unrelated disk credentials must not trigger exchange")
			f.noAccounts(t)
		})
	}
}

func TestSelectedTokenDurablePreflightRejectsInvalidJournal(t *testing.T) {
	for _, mode := range []string{"public-permissions", "symlink", "duplicate-field", "case-alias", "unknown-version", "oversized", "oversized-token", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstalledLineageFixture(t)
			store := f.load(t)
			path := selectedTokenLineagePath(store.globalDataPath, f.owner.ProviderID)
			data := []byte(`{"version":1,"sequence":0,"records":{}}`)
			ctx := t.Context()
			token := cloneOAuthToken(f.original)
			switch mode {
			case "public-permissions":
				require.NoError(t, os.WriteFile(path, data, 0o600))
				makeAuthenticationJournalPublic(t, path)
			case "symlink":
				require.NoError(t, os.Symlink(store.globalDataPath, path))
			case "duplicate-field":
				require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"version":1,"sequence":0,"records":{}}`), 0o600))
			case "case-alias":
				require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"Version":1,"sequence":0,"records":{}}`), 0o600))
			case "unknown-version":
				require.NoError(t, os.WriteFile(path, []byte(`{"version":3,"sequence":0,"records":{}}`), 0o600))
			case "oversized":
				file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
				require.NoError(t, err)
				require.NoError(t, file.Truncate(maxSelectedTokenLineageBytes+1))
				require.NoError(t, file.Close())
			case "oversized-token":
				token.Client.ClientSecret = strings.Repeat("x", maxRemoteOAuthTokenBytes)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before, err := os.ReadFile(store.globalDataPath)
			require.NoError(t, err)
			_, err = store.RefreshProviderOAuthTokenForRuntime(ctx, ScopeGlobal, f.owner, token, store.RuntimeSnapshot())
			require.Error(t, err)
			require.Zero(t, f.requests.Load())
			after, err := os.ReadFile(store.globalDataPath)
			require.NoError(t, err)
			require.Equal(t, before, after)
			f.noAccounts(t)
		})
	}
}

func TestSelectedTokenDurableCapacityPreservesUnresolvedAndCurrentPredecessor(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			f := newInstalledLineageFixture(t)
			store := f.load(t)
			digest, err := store.loadAuthenticationDigest(t.Context())
			require.NoError(t, err)
			fresh, err := store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, f.original, store.RuntimeSnapshot())
			require.NoError(t, err)
			path := selectedTokenLineagePath(store.globalDataPath, f.owner.ProviderID)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var journal selectedTokenLineageJournal
			require.NoError(t, json.Unmarshal(data, &journal))
			var predecessor string
			var template selectedTokenLineageRecord
			for key, record := range journal.Records {
				predecessor, template = key, record
			}
			require.True(t, template.Committed)
			// Populate the finite retention boundary from the actual serialized
			// receipt. These historical entries exercise eviction, not exchanges.
			for i := uint64(2); i <= selectedTokenRotationLimit; i++ {
				record := template
				record.Sequence = i
				record.Committed = committed
				record.Original = digest.bytesID(authenticationDigestCredential, []byte(fmt.Sprint("historical-start", i)))
				record.Successor = cloneOAuthToken(fresh)
				record.Successor.AccessToken = fmt.Sprint("historical-successor", i)
				journal.Records[digest.bytesID(authenticationDigestOperation, []byte(fmt.Sprint("historical-key", i)))] = record
			}
			journal.Sequence = selectedTokenRotationLimit
			data, err = json.Marshal(journal)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o600))
			next, err := store.RefreshProviderOAuthTokenForRuntime(t.Context(), ScopeGlobal, f.owner, fresh, store.RuntimeSnapshot())
			if !committed {
				require.ErrorContains(t, err, "unresolved")
				require.Nil(t, next)
				require.EqualValues(t, 1, f.requests.Load())
			} else {
				require.NoError(t, err)
				require.Equal(t, "installed-next-access", next.AccessToken)
				require.EqualValues(t, 2, f.requests.Load())
				data, err = os.ReadFile(path)
				require.NoError(t, err)
				journal = selectedTokenLineageJournal{}
				require.NoError(t, json.Unmarshal(data, &journal))
				require.Len(t, journal.Records, selectedTokenRotationLimit)
				require.Contains(t, journal.Records, predecessor)
				require.NotContains(t, journal.Records, digest.bytesID(authenticationDigestOperation, []byte("historical-key2")))
				require.EqualValues(t, selectedTokenRotationLimit+1, journal.Sequence)
			}
			f.noAccounts(t)
		})
	}
}
