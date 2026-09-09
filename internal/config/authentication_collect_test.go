package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationCollectionUsesExactCommittedCapture(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
	switched, err := f.store.SwitchAuthenticationAccount(t.Context(), f.scope, f.capture(t), f.owner, "second")
	require.NoError(t, err)
	foreign := filepath.Join(t.TempDir(), "must-not-read")
	t.Setenv("AI_CLI_DIR", foreign)
	proposal, err := f.store.CollectRemoteRuntimeForAuthentication(t.Context(), switched.After, 2, nil)
	require.NoError(t, err)
	require.Len(t, proposal.Credentials, 1)
	require.Equal(t, f.owner, proposal.Credentials[0].Owner)
	require.Equal(t, "second", proposal.Credentials[0].Account.ID)
	require.Equal(t, f.second.AccessToken, proposal.Credentials[0].Account.AccessToken)
	require.Same(t, f.store.Config(), proposal.CollectionConfig())
	require.NoDirExists(t, foreign)
	loggedOut, err := f.store.LogoutAuthentication(t.Context(), f.scope, switched.After, f.owner)
	require.NoError(t, err)
	proposal, err = f.store.CollectRemoteRuntimeForAuthentication(t.Context(), loggedOut.After, 3, map[providerregistry.RegistrationOwner]bool{f.owner: true})
	require.NoError(t, err)
	require.Len(t, proposal.Credentials, 1)
	require.True(t, proposal.Credentials[0].Unavailable)
	require.Nil(t, proposal.Credentials[0].Account)
	require.Empty(t, proposal.Credentials[0].APIKey)
	require.NoDirExists(t, foreign)
	_, err = f.store.CollectRemoteRuntimeForAuthentication(t.Context(), switched.After, 4, nil)
	require.Error(t, err, "an older successful receipt cannot collect a newer runtime")
}

func TestAuthenticationCollectionRejectsChangedObservationAndCanceledWait(t *testing.T) {
	for _, changed := range []string{"account", "config", "publication", "cancel", "empty", "zero revision"} {
		t.Run(changed, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			before := f.capture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			revision := uint64(2)
			switch changed {
			case "account":
				require.NoError(t, accounts.SaveWithoutActivating(ctx, f.owner.AccountNamespace, f.second))
			case "config":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
			case "publication":
				f.store.setConfig(f.store.Config())
			case "cancel":
				f.store.writeMu.Lock()
				defer f.store.writeMu.Unlock()
				cancel()
			case "empty":
				before = AuthenticationCapture{}
			case "zero revision":
				revision = 0
			}
			proposal, err := f.store.CollectRemoteRuntimeForAuthentication(ctx, before, revision, nil)
			require.Error(t, err)
			require.Empty(t, proposal.Credentials)
			require.Empty(t, proposal.Digest)
			require.Nil(t, proposal.CollectionConfig())
			if changed == "cancel" {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}

func TestAuthenticationCollectionChecksChangesAfterCancelableKeyEvaluation(t *testing.T) {
	for _, action := range []string{"cancel", "account", "config"} {
		t.Run(action, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			entered, release := filepath.Join(f.root, "key-entered"), filepath.Join(f.root, "key-release")
			// Install a retained expression in a fixture generation. The actual
			// collector and production shell resolver execute it below; no test
			// resolver or account-reader callback replaces that path.
			next := f.store.Config().cloneForWrite()
			for _, kind := range []SelectedModelType{SelectedModelTypeLarge, SelectedModelTypeSmall} {
				next.Models[kind] = SelectedModel{Provider: "unrelated", Model: "other"}
			}
			provider, _ := next.Providers.Get("unrelated")
			provider.APIKey = fmt.Sprintf("$(printf x > '%s'; while [ ! -f '%s' ]; do :; done; printf synthetic-unrelated)", filepath.ToSlash(entered), filepath.ToSlash(release))
			next.Providers.Set("unrelated", provider)
			f.store.setConfig(next)
			before := f.capture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type completed struct {
				proposal RemoteRuntimeProposal
				err      error
			}
			done := make(chan completed, 1)
			go func() {
				p, e := f.store.CollectRemoteRuntimeForAuthentication(ctx, before, 2, nil)
				done <- completed{p, e}
			}()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
		waiting:
			for {
				select {
				case <-ticker.C:
					if _, err := os.Stat(entered); err == nil {
						break waiting
					}
				case result := <-done:
					t.Fatalf("key command did not enter: %v", result.err)
				case <-ctx.Done():
					t.Fatal("key command did not enter")
				}
			}
			leaseCtx, leaseCancel := context.WithTimeout(t.Context(), time.Second)
			defer leaseCancel()
			_, err := accounts.Active(leaseCtx, f.owner.AccountNamespace)
			require.NoError(t, err, "key evaluation must not hold the account lease")
			switch action {
			case "cancel":
				cancel()
			case "account":
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.second))
			case "config":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
			}
			require.NoError(t, os.WriteFile(release, nil, 0o600))
			select {
			case result := <-done:
				require.Error(t, result.err)
				require.Empty(t, result.proposal.Credentials)
				require.Empty(t, result.proposal.Digest)
				if action == "cancel" {
					require.ErrorIs(t, result.err, context.Canceled)
				}
			case <-time.After(time.Second):
				t.Fatal("collection did not complete promptly")
			}
		})
	}
}
