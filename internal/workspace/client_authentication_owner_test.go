package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestClientAuthenticationMutationOriginalOwnerOnRealPartial(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(fmt.Sprint(logout), func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			target := f.target(t)
			commits := 0
			f.store.SetRuntimeGenerationPreparer(func(_ context.Context, _ config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
				return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() {
					commits++
					data, err := os.ReadFile(f.path)
					require.NoError(t, err)
					// Peer edit after publication makes the exact final observation
					// fail. Durable local effects remain true, with no coherent After.
					require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
				}}, nil
			})
			operation := strings.Repeat("c", 32)
			invoke := func() (providerauth.MutationOutcome, error) {
				if logout {
					return f.w.logoutClientAuthentication(t.Context(), providerauth.LogoutRequest{OperationID: operation, Target: target})
				}
				return f.w.switchClientAuthentication(t.Context(), providerauth.SwitchRequest{OperationID: operation, Target: target, AccountID: f.second.ID})
			}
			accepted := f.w.Config()
			outcome, err := invoke()
			require.ErrorIs(t, err, providerauth.ErrMutation)
			require.Equal(t, providerauth.MutationProgress{AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}, outcome.Progress)
			require.Nil(t, outcome.Change)
			receipt := f.w.authority.authenticationReceipts[operation]
			require.NotNil(t, receipt)
			require.Equal(t, f.owner, receipt.owner, "partial receipt retains admitted full owner without deriving it from After")
			require.False(t, receipt.after.SameObservation(receipt.after))
			require.Nil(t, receipt.proposal)
			require.False(t, receipt.acknowledged)
			require.False(t, receipt.adopted)
			require.Zero(t, f.puts.Load())
			require.Same(t, accepted, f.w.Config())
			require.False(t, f.w.authority.removed[f.owner])
			paths := []string{f.path, f.accountsPath}
			infos, bodies := clientAuthenticationFiles(t, paths...)
			again, err := invoke()
			require.ErrorIs(t, err, providerauth.ErrMutation)
			require.Equal(t, outcome, again)
			require.Equal(t, f.owner, receipt.owner)
			require.Equal(t, 1, commits)
			require.Zero(t, f.puts.Load())
			requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
			_, err = json.Marshal(receipt)
			require.Error(t, err)
			for _, verb := range []string{"%v", "%+v", "%#v"} {
				require.NotContains(t, fmt.Sprintf(verb, receipt), f.owner.AccountNamespace)
			}
			public, err := json.Marshal(outcome)
			require.NoError(t, err)
			require.NotContains(t, string(public), "account_namespace")
			require.NotContains(t, string(public), "originalOwner")
		})
	}
}

func TestClientAuthenticationMutationIntentOwnerRetainedOnAdmissionRefusal(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("d", 32), Target: f.target(t), AccountID: "missing-account"}
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	_, err := f.w.switchClientAuthentication(t.Context(), request)
	require.ErrorIs(t, err, providerauth.ErrAccount)
	receipt := f.w.authority.authenticationReceipts[request.OperationID]
	require.NotNil(t, receipt)
	require.Equal(t, f.owner, receipt.owner, "durable intent retains its exact selected owner independently of Service admission")
	require.True(t, receipt.localFinished)
	require.True(t, receipt.journalCompleted)
	require.False(t, clientAuthenticationChanged(receipt.outcome.Progress))
	require.Nil(t, receipt.outcome.Change)
	require.Nil(t, receipt.proposal)
	require.False(t, receipt.acknowledged)
	require.False(t, receipt.adopted)
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
	require.Zero(t, f.puts.Load())
}
