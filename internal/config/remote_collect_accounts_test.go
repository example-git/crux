package config

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/stretchr/testify/require"
)

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
