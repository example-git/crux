package config

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func clientRefreshRuntimeFixture(t *testing.T) (*ConfigStore, RemoteRuntimeProposal) {
	t.Helper()
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("codex")
	require.True(t, ok)
	proposal := sealRemoteRuntime(t, RemoteRuntimeProposal{Version: RemoteRuntimeVersion, Revision: 1,
		Providers:   []RemoteProviderDefinition{{Config: ProviderConfig{ID: "codex", Name: "Fixture", Type: catalog.TypeOpenAICompat, BaseURL: "wss://fixture.invalid/responses", Owner: &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: providerregistry.ConstructionCodex}, Models: []catalog.Model{{ID: "fixture", Name: "Fixture"}}}}},
		Models:      map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: "codex", Model: "fixture"}, SelectedModelTypeSmall: {Provider: "codex", Model: "fixture"}},
		Credentials: []RemoteCredentialBinding{{Owner: registration.Owner(), Generation: 1, Account: &accounts.Entry{ID: "selected", AccessToken: "synthetic-old", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}}},
	})
	root := t.TempDir()
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	return store, proposal
}

func TestClientRefreshCompletionRequiresExactAcceptedRotation(t *testing.T) {
	store, proposal := clientRefreshRuntimeFixture(t)
	owner := proposal.Credentials[0].Owner
	admitted := store.RuntimeSnapshot()
	requests := make(chan ClientRefreshRequest, 1)
	store.SetClientRefreshPublisher(func(_ context.Context, request ClientRefreshRequest) { requests <- request })
	result := make(chan RuntimeSnapshot, 1)
	errors := make(chan error, 1)
	go func() {
		snapshot, err := store.RequestClientRefresh(t.Context(), admitted, owner)
		result <- snapshot
		errors <- err
	}()
	request := <-requests
	require.Len(t, store.PendingClientRefreshes(), 1)
	require.ErrorContains(t, store.CompleteClientRefresh("wrong-principal", ClientRefreshCompletion{RequestID: request.ID, Failed: true}), "principal mismatch")
	response := ClientRefreshCompletion{RequestID: request.ID, Revision: 2, Digest: "unaccepted", CredentialID: "wrong"}
	require.ErrorContains(t, store.CompleteClientRefresh(request.Principal, response), "accepted account rotation")
	fresh := *proposal.Credentials[0].Account
	fresh.AccessToken, fresh.RefreshToken = "synthetic-new", "synthetic-new-refresh"
	proposal.Revision = 2
	proposal.Credentials[0].Generation = 2
	proposal.Credentials[0].Account = &fresh
	proposal = sealRemoteRuntime(t, proposal)
	_, err := store.ReplaceRemoteRuntime(t.Context(), proposal, request.Principal, 1)
	require.NoError(t, err)
	response.Digest = proposal.Digest
	response.CredentialID = accounts.CredentialID(fresh)
	require.NoError(t, store.CompleteClientRefresh(request.Principal, response))
	require.NoError(t, <-errors)
	accepted := <-result
	require.Equal(t, uint64(2), accepted.RemoteAuthority().Revision)
	require.NoError(t, store.CompleteClientRefresh(request.Principal, response), "lost completion acknowledgements are idempotent")
	require.Error(t, store.CompleteClientRefresh(request.Principal, ClientRefreshCompletion{RequestID: request.ID, Failed: true}))
	// A later account choice must not replace the captured refresh result.
	other := fresh
	other.ID, other.AccessToken = "other-account", "synthetic-other"
	proposal.Revision, proposal.Credentials[0].Generation = 3, 3
	proposal.Credentials[0].Account = &other
	proposal = sealRemoteRuntime(t, proposal)
	_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, request.Principal, 2)
	require.NoError(t, err)
	peer, err := store.RequestClientRefresh(t.Context(), admitted, owner)
	require.NoError(t, err)
	account, ok := peer.EphemeralAccount(owner)
	require.True(t, ok)
	require.Equal(t, fresh.ID, account.ID)
	require.Equal(t, fresh.AccessToken, account.AccessToken)
	require.Equal(t, accepted.RemoteAuthority(), peer.RemoteAuthority())
}

func TestClientRefreshCancellationRemovesPendingRequest(t *testing.T) {
	store, proposal := clientRefreshRuntimeFixture(t)
	requests := make(chan ClientRefreshRequest, 1)
	store.SetClientRefreshPublisher(func(_ context.Context, request ClientRefreshRequest) { requests <- request })
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := store.RequestClientRefresh(ctx, store.RuntimeSnapshot(), proposal.Credentials[0].Owner)
		result <- err
	}()
	request := <-requests
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	require.Empty(t, store.PendingClientRefreshes())
	require.ErrorContains(t, store.CompleteClientRefresh(request.Principal, ClientRefreshCompletion{RequestID: request.ID, Failed: true}), "not pending")
}

func TestClientRefreshCompletionRejectsChangedProviderDefinition(t *testing.T) {
	for _, field := range []string{"endpoint", "headers"} {
		t.Run(field, func(t *testing.T) {
			store, proposal := clientRefreshRuntimeFixture(t)
			requests := make(chan ClientRefreshRequest, 1)
			store.SetClientRefreshPublisher(func(_ context.Context, request ClientRefreshRequest) { requests <- request })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := store.RequestClientRefresh(ctx, store.RuntimeSnapshot(), proposal.Credentials[0].Owner)
				result <- err
			}()
			request := <-requests
			fresh := *proposal.Credentials[0].Account
			fresh.AccessToken, fresh.RefreshToken = "rotated-access", "rotated-refresh"
			proposal.Revision, proposal.Credentials[0].Generation = 2, 2
			proposal.Credentials[0].Account = &fresh
			if field == "endpoint" {
				proposal.Providers[0].Config.BaseURL = "wss://changed.invalid/responses"
			} else {
				proposal.Providers[0].Config.ExtraHeaders = map[string]string{"X-Workspace-Policy": "changed"}
			}
			proposal = sealRemoteRuntime(t, proposal)
			_, err := store.ReplaceRemoteRuntime(t.Context(), proposal, request.Principal, 1)
			require.NoError(t, err)
			err = store.CompleteClientRefresh(request.Principal, ClientRefreshCompletion{RequestID: request.ID, Revision: 2, Digest: proposal.Digest, CredentialID: accounts.CredentialID(fresh)})
			require.ErrorContains(t, err, "provider definition changed")
			require.Equal(t, proposal.Digest, store.RemoteAuthority().Digest, "rejecting the old completion must preserve the user's new accepted runtime")
			require.NoError(t, store.CompleteClientRefresh(request.Principal, ClientRefreshCompletion{RequestID: request.ID, Failed: true}))
			require.Error(t, <-result)
		})
	}
}

func TestClientRefreshFailureAndAccountSwitch(t *testing.T) {
	for _, action := range []string{"client-failed", "account-switched"} {
		t.Run(action, func(t *testing.T) {
			store, proposal := clientRefreshRuntimeFixture(t)
			requests := make(chan ClientRefreshRequest, 1)
			store.SetClientRefreshPublisher(func(_ context.Context, request ClientRefreshRequest) { requests <- request })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := store.RequestClientRefresh(ctx, store.RuntimeSnapshot(), proposal.Credentials[0].Owner)
				result <- err
			}()
			request := <-requests
			if action == "client-failed" {
				require.NoError(t, store.CompleteClientRefresh(request.Principal, ClientRefreshCompletion{RequestID: request.ID, Failed: true}))
				require.ErrorContains(t, <-result, "owning client could not refresh")
				return
			}
			other := *proposal.Credentials[0].Account
			other.ID, other.AccessToken = "different-account", "synthetic-other-token"
			proposal.Credentials[0].Account = &other
			proposal.Credentials[0].Generation, proposal.Revision = 2, 2
			proposal = sealRemoteRuntime(t, proposal)
			_, err := store.ReplaceRemoteRuntime(t.Context(), proposal, request.Principal, 1)
			require.NoError(t, err)
			require.ErrorContains(t, store.CompleteClientRefresh(request.Principal, ClientRefreshCompletion{RequestID: request.ID, Revision: 2, Digest: proposal.Digest, CredentialID: accounts.CredentialID(other)}), "accepted account rotation")
			require.Len(t, store.PendingClientRefreshes(), 1)
			cancel()
			require.ErrorIs(t, <-result, context.Canceled)
		})
	}
}
