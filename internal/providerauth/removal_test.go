package providerauth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthenticationRemovalReceiptBindsExactEffectAndReplay(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "inactive", true: "active"}[active], func(t *testing.T) {
			f := newMutationFixture(t)
			id := f.selected.ID
			if active {
				id = f.old.ID
			}
			request := RemoveRequest{OperationID: strings.Repeat("c", 32), Target: f.request.Target, AccountID: id}
			result, err := f.service.Remove(t.Context(), request)
			require.NoError(t, err)
			require.NoError(t, result.Outcome.ValidateRemove(request))
			require.Equal(t, id, result.Outcome.RemovedAccountID)
			require.Equal(t, active, result.Outcome.Progress.RuntimePublished)
			_, ok := result.AuthenticationCapture()
			require.True(t, ok)
			_, ok = result.RuntimeSnapshot()
			require.True(t, ok)
			successor, wasActive, ok := result.OriginalRemovalSelection()
			require.True(t, ok)
			require.Equal(t, active, wasActive)
			require.Equal(t, successor, result.Outcome.Change.Current.Status.ActiveAccountID)
			again, err := f.service.Remove(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, result.Outcome, again.Outcome)
			wrong := request
			wrong.AccountID = successor
			require.Error(t, result.Outcome.ValidateRemove(wrong))
			_, err = f.service.Remove(t.Context(), wrong)
			require.ErrorIs(t, err, ErrOperationConflict)
			require.Error(t, result.Outcome.ValidateSwitch(SwitchRequest(request)))
			forged := result.Outcome
			forged.RemovedAccountID = "other"
			require.Error(t, forged.ValidateRemove(request))
			stale := request
			stale.OperationID = strings.Repeat("d", 32)
			_, err = f.service.Remove(t.Context(), stale)
			require.ErrorIs(t, err, ErrStale)
		})
	}
}
