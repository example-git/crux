package providerauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

type originalOwnerMutation struct {
	mutationStub
	owner providerregistry.RegistrationOwner
}

func (m *originalOwnerMutation) SwitchAuthenticationAccount(ctx context.Context, scope config.Scope, before config.AuthenticationCapture, owner providerregistry.RegistrationOwner, id string) (config.AuthenticationMutationResult, error) {
	m.owner = owner
	return m.mutationStub.SwitchAuthenticationAccount(ctx, scope, before, owner, id)
}

func (m *originalOwnerMutation) LogoutAuthentication(ctx context.Context, scope config.Scope, before config.AuthenticationCapture, owner providerregistry.RegistrationOwner) (config.AuthenticationMutationResult, error) {
	m.owner = owner
	return m.mutationStub.LogoutAuthentication(ctx, scope, before, owner)
}

func TestAuthenticationMutationOriginalOwnerSurvivesPartialReplay(t *testing.T) {
	for _, logout := range []bool{false, true} {
		for _, progress := range []config.AuthenticationMutationResult{{}, {AccountRefreshed: true}, {AccountsSaved: true}, {AccountsSaved: true, ConfigSaved: true}, {AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}} {
			t.Run(fmt.Sprintf("logout=%t-%t-%t-%t-%t", logout, progress.AccountRefreshed, progress.AccountsSaved, progress.ConfigSaved, progress.RuntimePublished), func(t *testing.T) {
				f := newMutationFixture(t)
				mutator := &originalOwnerMutation{mutationStub: mutationStub{result: progress, err: errors.New("synthetic-partial")}}
				f.service.mutations = mutator
				invoke := func() (MutationResult, error) {
					if logout {
						return f.service.Logout(t.Context(), LogoutRequest{OperationID: f.request.OperationID, Target: f.request.Target})
					}
					return f.service.Switch(t.Context(), f.request)
				}
				result, err := invoke()
				require.ErrorIs(t, err, ErrMutation)
				owner, admitted := result.OriginalOwner()
				require.True(t, admitted)
				require.Equal(t, f.owner, owner)
				require.Equal(t, mutator.owner, owner, "the owner is the one passed to the admitted fixed transaction")
				require.Equal(t, MutationProgress{progress.AccountRefreshed, progress.AccountsSaved, progress.ConfigSaved, progress.RuntimePublished}, result.Outcome.Progress)
				require.Nil(t, result.Outcome.Change)
				_, current := result.AuthenticationCapture()
				require.False(t, current)

				// Replace the service's current store with an otherwise identical
				// registration whose private namespace differs. A public Target
				// cannot distinguish these, and must never reconstruct provenance.
				registration, ok := f.store.RuntimeSnapshot().ProviderRegistration(f.owner.ProviderID)
				require.True(t, ok)
				registration.AccountNamespace = "different-private-namespace"
				f.service.store = config.NewTestStoreWithRegistrations(f.store.Config(), registration)
				replacement, ok := f.service.store.RuntimeSnapshot().ProviderOwner(f.owner.ProviderID)
				require.True(t, ok)
				require.Equal(t, PublicOwner(owner), PublicOwner(replacement))
				require.NotEqual(t, owner, replacement)
				replayed, err := invoke()
				require.ErrorIs(t, err, ErrMutation)
				again, admitted := replayed.OriginalOwner()
				require.True(t, admitted)
				require.Equal(t, f.owner, again)
				require.Equal(t, 1, mutator.calls)
				owner.AccountNamespace = "caller-mutated-value"
				again, _ = result.OriginalOwner()
				require.Equal(t, f.owner, again, "accessor returns a value copy")
				for _, private := range []any{result, &result, f.service.receipts[f.request.OperationID]} {
					_, err := json.Marshal(private)
					require.Error(t, err)
					for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
						require.NotContains(t, fmt.Sprintf(verb, private), f.owner.AccountNamespace)
					}
				}
				public, err := json.Marshal(result.Outcome)
				require.NoError(t, err)
				// Integrated namespaces may equal public provider IDs. Check the
				// private field boundary, not an overlapping public name.
				require.NotContains(t, string(public), "account_namespace")
				require.NotContains(t, string(public), "originalOwner")
			})
		}
	}
}

func TestAuthenticationMutationOriginalOwnerAbsentBeforeAdmission(t *testing.T) {
	for _, mode := range []string{"workspace", "epoch", "sequence", "owner", "account", "operation-id", "cancel", "unavailable", "exhausted", "accepted-view", "id-conflict"} {
		t.Run(mode, func(t *testing.T) {
			f := newMutationFixture(t)
			mutator := &originalOwnerMutation{mutationStub: mutationStub{err: errors.New("admitted failure")}}
			f.service.mutations = mutator
			request := f.request
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "workspace":
				request.Target.WorkspaceID = "foreign"
			case "epoch":
				request.Target.Generation.Epoch = strings.Repeat("0", 32)
			case "sequence":
				request.Target.Generation.Sequence++
			case "owner":
				request.Target.Owner.OAuthFlowID = "foreign-flow"
			case "account":
				request.AccountID = "missing"
			case "operation-id":
				request.OperationID = "invalid"
			case "cancel":
				cancel()
			case "unavailable":
				f.service.mutations = nil
			case "exhausted":
				f.service.sequence = math.MaxUint64
				request.Target.Generation.Sequence = math.MaxUint64
			case "id-conflict":
				_, err := f.service.Switch(ctx, request)
				require.ErrorIs(t, err, ErrMutation)
				request.AccountID = f.old.ID
			}
			var result MutationResult
			var err error
			if mode == "accepted-view" {
				result, err = f.service.SwitchForAccepted(ctx, request, config.RemoteRuntimeProposal{}, f.store.Config())
			} else {
				result, err = f.service.Switch(ctx, request)
			}
			require.Error(t, err)
			owner, admitted := result.OriginalOwner()
			require.False(t, admitted)
			require.Empty(t, owner)
			if mode == "id-conflict" {
				require.Equal(t, 1, mutator.calls)
			} else {
				require.Zero(t, mutator.calls)
			}
		})
	}
}

func TestAuthenticationMutationOriginalOwnerOnSuccessfulHistoricalAndUnverifiedResults(t *testing.T) {
	f := newMutationFixture(t)
	result, err := f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	owner, admitted := result.OriginalOwner()
	require.True(t, admitted)
	require.Equal(t, f.owner, owner)

	// Break only receipt verification, after the operation was admitted and
	// completed. Its original owner is still evidence even without currentness.
	actual := f.service.store
	f.service.store = nil
	unverified, err := f.service.Switch(t.Context(), f.request)
	require.ErrorIs(t, err, ErrReceiptUnverified)
	owner, admitted = unverified.OriginalOwner()
	require.True(t, admitted)
	require.Equal(t, f.owner, owner)
	_, current := unverified.AuthenticationCapture()
	require.False(t, current)
	f.service.store = actual
	_, err = f.service.Logout(t.Context(), LogoutRequest{OperationID: strings.Repeat("2", 32), Target: result.Outcome.Change.Current.Target})
	require.NoError(t, err)
	historical, err := f.service.Switch(t.Context(), f.request)
	require.NoError(t, err)
	require.True(t, historical.Outcome.Superseded)
	owner, admitted = historical.OriginalOwner()
	require.True(t, admitted)
	require.Equal(t, f.owner, owner)
	_, current = historical.AuthenticationCapture()
	require.False(t, current)
}
