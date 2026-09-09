package config

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func TestClientProviderWithdrawalPublishesOnlyWithAcceptedRuntime(t *testing.T) {
	for _, mode := range []string{"prepare failure", "cancel after prepare", "revision rejection", "principal rejection"} {
		t.Run(mode, func(t *testing.T) {
			proposal := remoteRuntimeFixture(t, "minimal.plugin")
			owner := proposal.Credentials[0].Owner
			root, principal := t.TempDir(), strings.Repeat("a", 64)
			store, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, principal, env.NewFromMap(map[string]string{}))
			require.NoError(t, err)
			captured := store.RuntimeSnapshot()
			proposal.Revision, proposal.Credentials[0].Generation = 2, 2
			proposal.Credentials[0].APIKey, proposal.Credentials[0].Unavailable = "", true
			proposal = sealRemoteRuntime(t, proposal)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			prepared, aborted := 0, 0
			var candidate RuntimeSnapshot
			store.SetRuntimeGenerationPreparer(func(_ context.Context, value RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
				prepared++
				candidate = value
				require.EqualValues(t, 2, value.clientRuntime.withdrawnAt[owner])
				require.Empty(t, store.clientRuntime.withdrawnAt, "preparation must not mutate accepted history")
				if mode == "prepare failure" {
					return RuntimeGenerationCandidate{}, errors.New("synthetic withdrawal preparation failure")
				}
				cancel()
				return RuntimeGenerationCandidate{Commit: func() {}, Abort: func() { aborted++ }}, nil
			})
			expectedRevision, submittedPrincipal := uint64(1), principal
			if mode == "revision rejection" {
				expectedRevision = 0
			}
			if mode == "principal rejection" {
				submittedPrincipal = strings.Repeat("b", 64)
			}
			_, err = store.ReplaceRemoteRuntime(ctx, proposal, submittedPrincipal, expectedRevision)
			require.Error(t, err)
			require.Same(t, captured.Config(), store.Config())
			require.Empty(t, captured.clientRuntime.withdrawnAt)
			require.Empty(t, store.RuntimeSnapshot().clientRuntime.withdrawnAt)
			require.NoError(t, store.RuntimeSnapshot().ValidateClientProviderAdmission(captured, owner))
			if mode == "cancel after prepare" {
				require.Equal(t, 1, aborted)
			} else {
				require.Zero(t, aborted)
			}
			if strings.HasSuffix(mode, "rejection") {
				require.Zero(t, prepared)
			} else {
				require.Equal(t, 1, prepared)
			}
			// A subsequent ordinary available revision must not inherit the
			// rejected candidate's withdrawal.
			store.SetRuntimeGenerationPreparer(nil)
			proposal.Credentials[0].APIKey, proposal.Credentials[0].Unavailable = "synthetic-current", false
			proposal = sealRemoteRuntime(t, proposal)
			_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
			require.NoError(t, err)
			require.NoError(t, store.RuntimeSnapshot().ValidateClientProviderAdmission(captured, owner))
			if prepared != 0 {
				require.EqualValues(t, 2, candidate.clientRuntime.withdrawnAt[owner], "unpublished candidate history remains immutable")
			}
		})
	}
}

func TestClientProviderWithdrawalHistoryIsImmutableAcrossRestoration(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	owner := proposal.Credentials[0].Owner
	root, principal := t.TempDir(), strings.Repeat("a", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, principal, env.NewFromMap(map[string]string{}))
	require.NoError(t, err)
	original := store.RuntimeSnapshot()
	replace := func(unavailable bool) RuntimeSnapshot {
		previous := proposal.Revision
		proposal.Revision++
		proposal.Credentials[0].Generation = proposal.Revision
		proposal.Credentials[0].Unavailable = unavailable
		proposal.Credentials[0].APIKey = "synthetic-current"
		if unavailable {
			proposal.Credentials[0].APIKey = ""
		}
		proposal = sealRemoteRuntime(t, proposal)
		_, err := store.ReplaceRemoteRuntime(t.Context(), proposal, principal, previous)
		require.NoError(t, err)
		return store.RuntimeSnapshot()
	}
	withdrawn := replace(true)
	restored := replace(false)
	require.EqualValues(t, 2, restored.clientRuntime.withdrawnAt[owner])
	require.ErrorContains(t, restored.ValidateClientProviderAdmission(original, owner), "was withdrawn")
	require.NoError(t, restored.ValidateClientProviderAdmission(restored, owner))
	replace(true)
	latest := replace(false)
	require.EqualValues(t, 4, latest.clientRuntime.withdrawnAt[owner])
	require.EqualValues(t, 2, restored.clientRuntime.withdrawnAt[owner])
	require.EqualValues(t, 2, withdrawn.clientRuntime.withdrawnAt[owner])
	require.Empty(t, original.clientRuntime.withdrawnAt)
	require.ErrorContains(t, latest.ValidateClientProviderAdmission(restored, owner), "was withdrawn")
	require.NoError(t, latest.ValidateClientProviderAdmission(latest, owner))
	// Compilation has no receiver history. The guarantee intentionally applies
	// to one retained workspace, not an ABA-safe restart/creation protocol.
	fresh, err := CompileRemoteRuntime(root, filepath.Join(root, "fresh"), false, proposal, principal, env.NewFromMap(map[string]string{}))
	require.NoError(t, err)
	require.Empty(t, fresh.RuntimeSnapshot().clientRuntime.withdrawnAt)
}
