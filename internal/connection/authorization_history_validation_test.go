package connection

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAuthorizationHistoryReservesRevocationAndPreservesUnknownDrain(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	// Seed real distinct identities in one ordinary store transaction, then
	// exercise all admission, revoke, retry, abandonment and pruning publicly.
	require.NoError(t, update(t.Context(), func(data *store) error {
		for i := range authorizationReservationLimit {
			name := fmt.Sprintf("reserved-%03d", i)
			identity, err := NewClientIdentity(name)
			if err != nil {
				return err
			}
			cert, err := authorizationRecordCertificate(identity.Certificate)
			if err != nil {
				return err
			}
			data.AuthorizedClients[name] = identity.Certificate
			recordAuthorization(t.Context(), data, name, certificateFingerprint(cert))
		}
		return nil
	}))
	replacement, err := NewClientIdentity("replacement")
	require.NoError(t, err)
	before, err := os.ReadFile(storePath())
	require.NoError(t, err)
	require.ErrorContains(t, AuthorizeClient(t.Context(), "overflow", replacement.Certificate), "capacity")
	after, err := os.ReadFile(storePath())
	require.NoError(t, err)
	require.True(t, string(before) == string(after), "capacity refusal must not rewrite grants")

	// A stale local registration is deliberately not a successful drain.
	// Capture its instance first; removing the file later must preserve that
	// unknown result instead of turning the receipt into an empty-daemon case.
	dir := authorizationDaemonDir(storePath())
	require.NoError(t, os.MkdirAll(dir, 0o700))
	instance := uuid.NewString()
	registration := filepath.Join(dir, instance+".json")
	require.NoError(t, os.WriteFile(registration, []byte(`{}`), 0o600))
	outcome, err := RevokeClientWithOutcome(t.Context(), "reserved-000", "")
	require.ErrorContains(t, err, "unacknowledged")
	require.True(t, outcome.Saved, "reserved history must allow removal even at capacity")
	require.Equal(t, "pending", outcome.Resolution)
	require.Len(t, outcome.Daemons, 1)
	require.Equal(t, instance, outcome.Daemons[0].InstanceID)
	require.False(t, outcome.Daemons[0].Acknowledged)
	original := outcome.RevocationRecord
	require.NoError(t, os.Remove(registration))
	outcome, err = RevokeClientWithOutcome(t.Context(), "reserved-000", original.OperationID)
	require.ErrorContains(t, err, "unacknowledged")
	require.Equal(t, original, outcome.RevocationRecord)
	require.Len(t, outcome.Daemons, 1)
	require.False(t, outcome.Daemons[0].Acknowledged)
	history, err := ListRevocationHistory(t.Context())
	require.NoError(t, err)
	require.Equal(t, authorizationReservationLimit-1, history.ActiveGrants)
	require.Equal(t, 1, history.Unresolved)
	require.False(t, history.OverBudget)
	require.NoError(t, PruneAuthorizationHistory(t.Context()))
	history, err = ListRevocationHistory(t.Context())
	require.NoError(t, err)
	require.Len(t, history.Revocations, 1, "pruning must retain unresolved receipts")
	require.ErrorContains(t, AuthorizeClient(t.Context(), "reserved-000", replacement.Certificate), "capacity")
	require.Error(t, AbandonRevocationAcknowledgement(t.Context(), "wrong-name", original.OperationID))
	require.NoError(t, AbandonRevocationAcknowledgement(t.Context(), "reserved-000", original.OperationID))
	require.NoError(t, AuthorizeClient(t.Context(), "reserved-000", replacement.Certificate))
	outcome, err = RevokeClientWithOutcome(t.Context(), "reserved-000", original.OperationID)
	require.ErrorContains(t, err, "explicitly abandoned")
	require.Equal(t, original, outcome.RevocationRecord)
	require.Equal(t, "abandoned", outcome.Resolution)
	data, err := load(t.Context())
	require.NoError(t, err)
	require.True(t, data.AuthorizedClients["reserved-000"] == replacement.Certificate, "old receipt retry must preserve the replacement grant")
	require.NoError(t, PruneAuthorizationHistory(t.Context()))
	history, err = ListRevocationHistory(t.Context())
	require.NoError(t, err)
	require.Empty(t, history.Revocations)
	require.Equal(t, authorizationReservationLimit, history.ActiveGrants)
	require.Zero(t, history.Unresolved)
	data, err = load(t.Context())
	require.NoError(t, err)
	require.True(t, data.AuthorizedClients["reserved-000"] == replacement.Certificate)
}
