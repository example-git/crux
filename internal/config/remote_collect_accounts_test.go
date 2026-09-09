package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/stretchr/testify/require"
)

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
			require.ErrorContains(t, err, "selected client account changed")
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
