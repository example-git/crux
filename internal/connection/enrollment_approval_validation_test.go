package connection

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnrollmentApprovalObservesDurableIdentityBeforeCommit(t *testing.T) {
	for _, mode := range []string{"approve", "deny", "cancel-after-review"} {
		t.Run(mode, func(t *testing.T) {
			setConnectionRoot(t, t.TempDir())
			_, err := EnsureServerIdentity(t.Context())
			require.NoError(t, err)
			before, err := os.ReadFile(storePath())
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type observation struct {
				candidate EnrollmentCandidate
				pending   []PendingPairing
				grants    []AuthorizedClient
				err       error
			}
			observed := make(chan observation, 1)
			approve := func(ctx context.Context, candidate EnrollmentCandidate) error {
				pending, pendingErr := ListPendingPairings(ctx)
				grants, grantErr := ListAuthorizedClients(ctx)
				observed <- observation{candidate, pending, grants, errors.Join(pendingErr, grantErr)}
				switch mode {
				case "deny":
					return errors.New("private local review denial detail")
				case "cancel-after-review":
					cancel()
				}
				// A callback's local changes cannot substitute a different
				// identity after the retained candidate was shown.
				candidate.ClientName = "substituted-local-copy"
				candidate.ClientFingerprint = "substituted-local-copy"
				return nil
			}
			enrollment, err := StartEnrollment(ctx, "tcp://127.0.0.1:0", "", time.Minute, approve)
			require.NoError(t, err)
			t.Cleanup(func() { _ = enrollment.Close() })
			saved, pairErr := Pair(t.Context(), "reviewed-client", enrollment.SetupCode())
			var seen observation
			select {
			case seen = <-observed:
			case <-time.After(5 * time.Second):
				t.Fatal("approval callback did not observe the pending identity")
			}
			require.NoError(t, seen.err)
			require.Empty(t, seen.grants, "no authorization may precede approval")
			require.Len(t, seen.pending, 1)
			require.Equal(t, "reviewed-client", seen.candidate.ClientName)
			require.Equal(t, seen.pending[0].ClientFingerprint, seen.candidate.ClientFingerprint)
			require.Equal(t, seen.pending[0].ServerFingerprint, seen.candidate.ServerFingerprint)
			require.Equal(t, enrollment.Address(), seen.candidate.Endpoint)
			require.Equal(t, time.Unix(enrollment.setup.ExpiresAt, 0), seen.candidate.ExpiresAt)
			waitCtx, waitCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer waitCancel()
			result, waitErr := enrollment.Wait(waitCtx)
			if mode == "approve" {
				require.NoError(t, pairErr)
				require.NoError(t, waitErr)
				require.Equal(t, "reviewed-client", saved.Name)
				require.Equal(t, seen.candidate.ClientFingerprint, result.Fingerprint)
				records, err := ListAuthorizationRecords(t.Context())
				require.NoError(t, err)
				require.Len(t, records, 1)
				require.Equal(t, seen.candidate.ClientFingerprint, records[0].Fingerprint)
				require.NotNil(t, records[0].ApprovedAt)
			} else {
				require.Error(t, pairErr)
				require.NotContains(t, pairErr.Error(), "private local review denial detail")
				require.Error(t, waitErr)
				after, err := os.ReadFile(storePath())
				require.NoError(t, err)
				require.True(t, string(before) == string(after), "denied or canceled review must preserve exact authorization bytes")
				pending, err := ListPendingPairings(t.Context())
				require.NoError(t, err)
				require.Len(t, pending, 1, "failed review must not silently delete the retained key")
				require.NoError(t, ForgetPendingPairing(t.Context(), pending[0].OperationID))
			}
		})
	}
}
