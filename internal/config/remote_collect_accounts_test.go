package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/stretchr/testify/require"
)

func TestLoadRemoteClientBindsActiveAccountAndDetectsLaterSwitch(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		t.Setenv(key, filepath.Join(root, key))
		require.NoError(t, os.MkdirAll(os.Getenv(key), 0o700))
	}
	t.Setenv("CRUX_PROVIDER_PROFILE", "plugin-compat")
	require.NoError(t, registrytest.Install(t.Context(), os.Getenv("CRUX_GLOBAL_DATA"), os.Getenv("CRUX_CACHE_DIR"), *registrytest.Provider("codex").Manifest))
	data := []byte(`{"providers":{"codex":{"api_key":"synthetic-stale-access","oauth":{"access_token":"synthetic-stale-access","refresh_token":"synthetic-stale-refresh"},"plugin":{"id":"test.codex","version":"1.1.0"},"owner":{"type":"plugin","construction":"integrated-codex","compatibility_adapter":"integrated-codex"},"models":[{"id":"fixture","name":"Fixture"}]}},"models":{"large":{"provider":"codex","model":"fixture"},"small":{"provider":"codex","model":"fixture"}}}`)
	require.NoError(t, os.WriteFile(GlobalConfigData(), data, 0o600))
	active := accounts.Entry{ID: "active", AccessToken: "synthetic-active-access", RefreshToken: "synthetic-active-refresh"}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, active))

	store, err := LoadRemoteClient(false)
	require.NoError(t, err)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, proposal.Credentials, 1)
	require.Equal(t, &active, proposal.Credentials[0].Account)

	replacement := accounts.Entry{ID: "replacement", AccessToken: "synthetic-replacement-access", RefreshToken: "synthetic-replacement-refresh"}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, replacement))
	_, err = store.CollectRemoteRuntime(t.Context(), 2)
	require.ErrorContains(t, err, "selected client account for provider \"codex\" changed; reload client configuration before reconnecting")
}

func TestRemoteClientSwitchingProviderBindsItsActiveAccountBeforeCollection(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		t.Setenv(key, filepath.Join(root, key))
		require.NoError(t, os.MkdirAll(os.Getenv(key), 0o700))
	}
	t.Setenv("CRUX_PROVIDER_PROFILE", "plugin-compat")
	require.NoError(t, registrytest.Install(t.Context(), os.Getenv("CRUX_GLOBAL_DATA"), os.Getenv("CRUX_CACHE_DIR"), *registrytest.Provider("codex").Manifest))
	require.NoError(t, registrytest.Install(t.Context(), os.Getenv("CRUX_GLOBAL_DATA"), os.Getenv("CRUX_CACHE_DIR"), *registrytest.Provider("gemini-ag").Manifest))
	data := []byte(`{"providers":{
		"codex":{"api_key":"synthetic-stale-codex-access","oauth":{"access_token":"synthetic-stale-codex-access","refresh_token":"synthetic-stale-codex-refresh"},"plugin":{"id":"test.codex","version":"1.1.0"},"owner":{"type":"plugin","construction":"integrated-codex","compatibility_adapter":"integrated-codex"},"models":[{"id":"fixture","name":"Fixture"}]},
		"gemini-ag":{"api_key":"synthetic-gemini-access","oauth":{"access_token":"synthetic-gemini-access","refresh_token":"synthetic-gemini-refresh"},"plugin":{"id":"test.gemini-ag","version":"1.1.0"},"owner":{"type":"plugin","construction":"integrated-gemini-antigravity","compatibility_adapter":"integrated-gemini-antigravity"},"models":[{"id":"fixture","name":"Fixture"}]}},
		"models":{"large":{"provider":"gemini-ag","model":"fixture"},"small":{"provider":"gemini-ag","model":"fixture"}}}`)
	require.NoError(t, os.WriteFile(GlobalConfigData(), data, 0o600))
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderGemini, accounts.Entry{ID: "gemini", AccessToken: "synthetic-gemini-access", RefreshToken: "synthetic-gemini-refresh"}))
	codexActive := accounts.Entry{ID: "codex-active", AccessToken: "synthetic-active-codex-access", RefreshToken: "synthetic-active-codex-refresh"}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, codexActive))

	store, err := LoadRemoteClient(false)
	require.NoError(t, err)
	// Only the provider selected at load time is bound; codex still carries the
	// stale config token because it was not selected.
	stale, ok := store.Config().Providers.Get("codex")
	require.True(t, ok)
	require.Equal(t, "synthetic-stale-codex-access", stale.OAuthToken.AccessToken)

	owner, ok := store.RuntimeSnapshot().ProviderOwner("codex")
	require.True(t, ok)
	for _, modelType := range []SelectedModelType{SelectedModelTypeLarge, SelectedModelTypeSmall} {
		_, err = store.UpdatePreferredModelForOwner(ScopeGlobal, modelType, SelectedModel{Provider: "codex", Model: "fixture"}, owner)
		require.NoError(t, err)
	}

	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err, "switching to a provider at runtime must publish it, not refuse collection")
	require.Equal(t, "codex", proposal.Models[SelectedModelTypeLarge].Provider)
	require.Len(t, proposal.Credentials, 1)
	require.Equal(t, &codexActive, proposal.Credentials[0].Account)
	bound, ok := store.Config().Providers.Get("codex")
	require.True(t, ok)
	require.Equal(t, codexActive.AccessToken, bound.OAuthToken.AccessToken)
}

func TestCollectRemoteRuntimeRetainsUnconfiguredOAuthCandidate(t *testing.T) {
	f := newAuthenticationCandidateFixture(t, "example-responses", false, false, "")
	next := f.store.Config().cloneForWrite()
	next.Models = map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: f.owner.ProviderID, Model: "example-reasoner"}, SelectedModelTypeSmall: {Provider: f.owner.ProviderID, Model: "example-small"}}
	f.store.setConfig(next)
	before, err := os.ReadFile(filepath.Join(f.root, "crux.json"))
	require.NoError(t, err)
	proposal, err := f.store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, proposal.Providers, 1)
	require.Len(t, proposal.Credentials, 1)
	require.True(t, proposal.Credentials[0].Unavailable)
	require.Empty(t, proposal.Credentials[0].APIKey)
	require.Nil(t, proposal.Credentials[0].Account)
	require.Equal(t, "captured-client", proposal.Providers[0].Config.Configuration["oauth_client_id"])
	require.Equal(t, "command-header", proposal.Providers[0].Config.ExtraHeaders["X-Once"])
	_, configured := f.store.Config().Providers.Get(f.owner.ProviderID)
	require.False(t, configured)
	require.False(t, f.store.Config().CanInitializeAgent())
	capture, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.NoError(t, capture.ValidateAcceptedAuthentication(proposal, proposal.CollectionConfig()))
	require.NoFileExists(t, f.marker, "collection/status must reuse prepared headers")
	after, err := os.ReadFile(filepath.Join(f.root, "crux.json"))
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Empty(t, f.store.Config().RedactedForTransport().authenticationCandidates)
	definition, _, err := f.store.RuntimeSnapshot().ClientProviderDefinition(f.owner.ProviderID)
	require.NoError(t, err)
	definition.Config.Configuration["oauth_client_id"] = "caller-change"
	unchanged, _, err := f.store.RuntimeSnapshot().ClientProviderDefinition(f.owner.ProviderID)
	require.NoError(t, err)
	require.Equal(t, "captured-client", unchanged.Config.Configuration["oauth_client_id"])
}

func TestCollectRemoteRuntimeReadsCurrentCapturedAccountPath(t *testing.T) {
	for _, change := range []string{"token", "active account"} {
		t.Run(change, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeGlobal, false)
			foreign := filepath.Join(t.TempDir(), "must-not-read")
			t.Setenv("AI_CLI_DIR", foreign)
			t.Setenv("HOME", foreign)
			t.Setenv("USERPROFILE", foreign)
			proposal, err := f.store.CollectRemoteRuntime(t.Context(), 1)
			require.NoError(t, err)
			require.Len(t, proposal.Credentials, 1)
			require.Equal(t, f.owner, proposal.Credentials[0].Owner)
			require.Equal(t, &f.first, proposal.Credentials[0].Account)
			require.NoDirExists(t, foreign)
			// A supported peer write to the captured store must still be seen.
			// Captured construction entries alone cannot establish freshness.
			t.Setenv("AI_CLI_DIR", filepath.Join(f.root, "accounts"))
			changed := f.first
			changed.AccessToken = "synthetic-peer-replacement"
			if change == "active account" {
				changed = f.second
			}
			require.NoError(t, accounts.Save(t.Context(), f.owner.AccountNamespace, changed))
			t.Setenv("AI_CLI_DIR", foreign)
			_, err = f.store.CollectRemoteRuntime(t.Context(), 2)
			require.ErrorContains(t, err, "selected client account for provider \""+f.owner.ProviderID+"\" changed")
			require.NoDirExists(t, foreign)
		})
	}
}

func TestCollectRemoteRuntimeInitialWaitIsCancelable(t *testing.T) {
	for _, held := range []string{"publication", "snapshot"} {
		t.Run(held, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeGlobal, false)
			lock, unlock := f.store.writeMu.Lock, f.store.writeMu.Unlock
			if held == "snapshot" {
				lock, unlock = f.store.configMu.Lock, f.store.configMu.Unlock
			}
			lock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := f.store.CollectRemoteRuntime(ctx, 1)
				done <- err
			}()
			select {
			case err := <-done:
				unlock()
				t.Fatalf("collection bypassed held snapshot lock: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			cancel()
			select {
			case err := <-done:
				unlock()
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				unlock()
				<-done
				t.Fatal("canceled collection waited for initial lock")
			}
		})
	}
}
