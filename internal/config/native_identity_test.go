package config

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

type nativeIdentityTransport func(*http.Request) (*http.Response, error)

func (f nativeIdentityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func nativeIdentityTLS(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	original, originalTransport := http.DefaultClient, http.DefaultTransport
	http.DefaultClient = &http.Client{Transport: nativeIdentityTransport(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		address := *r.URL
		address.Scheme, address.Host = target.Scheme, target.Host
		copy.URL = &address
		return server.Client().Transport.RoundTrip(copy)
	})}
	http.DefaultTransport = http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient = original; http.DefaultTransport = originalTransport })
	return server
}

func nativeIdentityLocalStore(t *testing.T, values map[string]string) *ConfigStore {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", root)
	values["AI_CLI_DIR"], values["HOME"], values["USERPROFILE"] = root, root, root
	entry := accounts.Entry{ID: "selected", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, entry))
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig](), Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: "codex", Model: "fixture"}, SelectedModelTypeSmall: {Provider: "codex", Model: "fixture"}}}
	cfg.setDefaults(root, filepath.Join(root, "data"))
	cfg.Providers.Set("codex", ProviderConfig{ID: "codex", APIKey: entry.AccessToken, OAuthToken: entry.Token(), BaseURL: "wss://fixture.invalid/responses", Type: catalog.TypeOpenAICompat, Owner: &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: providerregistry.ConstructionCodex}, Models: []catalog.Model{{ID: "fixture", Name: "Fixture"}}})
	store := NewTestStore(cfg)
	store.baseEnvironment = env.NewFromMap(values)
	store.effectiveEnvironment = cloneEnvironment(store.baseEnvironment)
	return store
}

func TestNativeIdentityCollectionRetainsEnvironmentAndDigest(t *testing.T) {
	for _, mode := range []string{"captured", "absent"} {
		t.Run(mode, func(t *testing.T) {
			values := map[string]string{"CODEX_VERSION": "1.2.3"}
			origin, terminal := "codex_cli_rs", "unknown"
			if mode == "captured" {
				values["CODEX_INTERNAL_ORIGINATOR_OVERRIDE"], values["TERM_PROGRAM"], values["TERM_PROGRAM_VERSION"] = "owner-client", "OwnerTerminal", "4.5"
				origin, terminal = "owner-client", "OwnerTerminal/4.5"
			}
			store := nativeIdentityLocalStore(t, values)
			captured := store.RuntimeSnapshot()
			_, ready := captured.nativeIdentities.peek(providerregistry.ConstructionCodex)
			require.False(t, ready)
			t.Setenv("CODEX_VERSION", "9.9.9")
			t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "hostile-originator")
			t.Setenv("TERM_PROGRAM", "HostileTerminal")
			t.Setenv("TERM_PROGRAM_VERSION", "9.9")
			proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
			require.NoError(t, err)
			identity := proposal.Providers[0].NativeIdentity
			require.NotNil(t, identity)
			require.Equal(t, "1.2.3", identity.Version)
			require.Equal(t, origin, identity.Originator)
			require.True(t, strings.HasPrefix(identity.UserAgent, origin+"/1.2.3 ("), identity.UserAgent)
			require.True(t, strings.HasSuffix(identity.UserAgent, " "+terminal), identity.UserAgent)
			definition, _, err := captured.ClientProviderDefinition("codex")
			require.NoError(t, err)
			require.Equal(t, proposal.Providers[0], definition)
			require.Same(t, captured.nativeIdentities, store.RuntimeSnapshot().nativeIdentities)
			observation, err := store.CaptureAuthentication(t.Context())
			require.NoError(t, err)
			require.NoError(t, observation.ValidateAcceptedAuthentication(proposal, proposal.CollectionConfig()))
			view := proposal.CollectionConfig()
			tampered := proposal
			tampered.collectionSource = nil
			tampered.Providers = append([]RemoteProviderDefinition(nil), proposal.Providers...)
			changed := *identity
			changed.UserAgent += " altered"
			tampered.Providers[0].NativeIdentity = &changed
			tampered = sealRemoteRuntime(t, tampered)
			require.ErrorContains(t, observation.ValidateAcceptedAuthentication(tampered, view), "not acknowledged")
			principal := strings.Repeat("a", 64)
			remote, err := CompileRemoteRuntime(t.TempDir(), t.TempDir(), false, proposal, principal, env.NewFromMap(map[string]string{"CODEX_VERSION": "8.8.8"}))
			require.NoError(t, err)
			old := remote.RuntimeSnapshot()
			admitted, err := old.ClientNativeIdentity("codex")
			require.NoError(t, err)
			require.Equal(t, *identity, admitted)
			admitted.UserAgent = "caller mutation"
			actual, err := old.ClientNativeIdentity("codex")
			require.NoError(t, err)
			require.Equal(t, *identity, actual)
			inputIdentity := *identity
			identity.UserAgent = "mutation after admission"
			actual, err = old.ClientNativeIdentity("codex")
			require.NoError(t, err)
			require.Equal(t, inputIdentity, actual, "admission must own its identity declaration")
			*identity = inputIdentity
			remote.RegisterRemoteRuntimeSecrets()
			require.Equal(t, redact.Replacement, redact.String(identity.UserAgent))
			for _, public := range []any{remote.Config(), remote.RemoteAuthority()} {
				data, err := json.Marshal(public)
				require.NoError(t, err)
				require.NotContains(t, string(data), identity.UserAgent)
				require.NotContains(t, string(data), "native_identity")
			}
			values["CODEX_VERSION"] = "2.3.4"
			store.writeMu.Lock()
			store.effectiveEnvironment = env.NewFromMap(values)
			store.setConfig(store.Config().cloneForWrite())
			store.writeMu.Unlock()
			next, err := store.CollectRemoteRuntime(t.Context(), 2)
			require.NoError(t, err)
			require.Equal(t, "2.3.4", next.Providers[0].NativeIdentity.Version)
			require.NotEqual(t, proposal.Digest, next.Digest)
			_, err = remote.ReplaceRemoteRuntime(t.Context(), next, principal, 1)
			require.NoError(t, err)
			current, err := remote.RuntimeSnapshot().ClientNativeIdentity("codex")
			require.NoError(t, err)
			require.Equal(t, "2.3.4", current.Version)
			retained, err := old.ClientNativeIdentity("codex")
			require.NoError(t, err)
			require.Equal(t, *identity, retained)
			cold, _, err := captured.ClientProviderDefinition("codex")
			require.NoError(t, err)
			require.Equal(t, definition, cold)
		})
	}
}

func TestNativeIdentityDiscoveryCancellationCanRetryAndDoesNotHoldConfigLocks(t *testing.T) {
	var calls atomic.Int32
	entered, canceled := make(chan struct{}), make(chan struct{})
	nativeIdentityTLS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/openai/codex/releases" {
			t.Errorf("unexpected discovery %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-r.Context().Done()
			close(canceled)
			return
		}
		_, _ = io.WriteString(w, `[{"tag_name":"rust-v3.4.5","prerelease":false}]`)
	})
	store := nativeIdentityLocalStore(t, map[string]string{})
	captured := store.RuntimeSnapshot()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := store.CollectRemoteRuntime(ctx, 1); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("discovery did not start")
	}
	require.True(t, store.writeMu.TryLock(), "discovery must not hold config lock")
	store.writeMu.Unlock()
	waiter, stop := context.WithCancel(t.Context())
	stop()
	_, err := captured.nativeIdentities.resolve(waiter, providerregistry.ConstructionCodex)
	require.ErrorIs(t, err, context.Canceled)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("request cancellation did not reach HTTPS server")
	}
	_, ready := captured.nativeIdentities.peek(providerregistry.ConstructionCodex)
	require.False(t, ready)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "3.4.5", proposal.Providers[0].NativeIdentity.Version)
	again, _, err := store.RuntimeSnapshot().ClientProviderDefinition("codex")
	require.NoError(t, err)
	require.Equal(t, proposal.Providers[0], again)
	require.Equal(t, int32(2), calls.Load(), "warm capture must not rediscover metadata")
}

func TestNativeIdentityAdmissionRejectsMissingInvalidAndUnrelated(t *testing.T) {
	_, base := clientRefreshRuntimeFixture(t)
	for _, kind := range []string{"missing", "control", "oversized", "mismatched-version", "mismatched-originator"} {
		t.Run(kind, func(t *testing.T) {
			var proposal RemoteRuntimeProposal
			data, err := json.Marshal(base)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &proposal))
			identity := proposal.Providers[0].NativeIdentity
			switch kind {
			case "missing":
				proposal.Providers[0].NativeIdentity = nil
			case "control":
				identity.UserAgent += "\r\nInjected: value"
			case "oversized":
				identity.UserAgent += strings.Repeat("x", 2048)
			case "mismatched-version":
				identity.Version = "9.9.9"
			case "mismatched-originator":
				identity.Originator = "other"
			}
			proposal = sealRemoteRuntime(t, proposal)
			_, err = CompileRemoteRuntime(t.TempDir(), t.TempDir(), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{}))
			require.ErrorContains(t, err, "native")
		})
	}
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	proposal.Providers[0].NativeIdentity = base.Providers[0].NativeIdentity
	proposal = sealRemoteRuntime(t, proposal)
	_, err := CompileRemoteRuntime(t.TempDir(), t.TempDir(), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{}))
	require.ErrorContains(t, err, "native identity does not match")
}

func TestNativeIdentityFailedProspectiveCaptureDoesNotEvictAcceptedValue(t *testing.T) {
	store := nativeIdentityLocalStore(t, map[string]string{"CODEX_VERSION": "1.2.3"})
	accepted := store.RuntimeSnapshot()
	expected, _, err := accepted.ClientProviderDefinition("codex")
	require.NoError(t, err)
	store.writeMu.Lock()
	store.configMu.Lock()
	prospective := store.runtimeSnapshotLocked(store.config.cloneForWrite(), store.resolver, store.providerRegistry, env.NewFromMap(map[string]string{"CODEX_VERSION": "9.9.9"}))
	store.configMu.Unlock()
	store.writeMu.Unlock()
	require.NotSame(t, accepted.nativeIdentities, prospective.nativeIdentities)
	actual := store.RuntimeSnapshot()
	require.Same(t, accepted.nativeIdentities, actual.nativeIdentities)
	definition, _, err := actual.ClientProviderDefinition("codex")
	require.NoError(t, err)
	require.Equal(t, expected, definition)
}
