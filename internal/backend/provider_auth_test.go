package backend

import (
	"context"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestProviderAuthRejectsUnadmittedWorkBeforeServiceCreation(t *testing.T) {
	for _, mode := range []string{"request canceled", "workspace canceled", "workspace closing", "workspace missing"} {
		t.Run(mode, func(t *testing.T) {
			b := New(t.Context(), nil, func() {})
			ws := &Workspace{ID: "fixture"}
			InsertWorkspaceForTest(b, ws)
			defer ws.cancel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			id := ws.ID
			var expected error
			switch mode {
			case "request canceled":
				cancel()
				expected = context.Canceled
			case "workspace canceled":
				ws.cancel()
				expected = context.Canceled
			case "workspace closing":
				ws.runMu.Lock()
				ws.closing = true
				ws.runMu.Unlock()
				expected = ErrWorkspaceClosing
			case "workspace missing":
				id = "missing"
				expected = ErrWorkspaceNotFound
			}
			_, err := b.ProviderAuthentication(ctx, id)
			require.ErrorIs(t, err, expected)
			_, err = b.ProviderAccounts(ctx, id, providerauth.Target{})
			require.ErrorIs(t, err, expected)
			target := providerauth.Target{WorkspaceID: id, Owner: providerauth.Owner{ProviderID: "fixture", HasOAuth: true}, Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 1}}
			switched, err := b.SwitchProviderAccount(ctx, id, providerauth.SwitchRequest{OperationID: strings.Repeat("b", 32), Target: target, AccountID: "selected"})
			require.ErrorIs(t, err, expected)
			require.Equal(t, target, switched.Outcome.Previous)
			require.Zero(t, switched.Outcome.Progress)
			require.Nil(t, switched.Workspace)
			cleared, err := b.LogoutProvider(ctx, id, providerauth.LogoutRequest{OperationID: strings.Repeat("c", 32), Target: target})
			require.ErrorIs(t, err, expected)
			require.Zero(t, cleared.Outcome.Progress)
			require.Nil(t, cleared.Workspace)
			require.Nil(t, ws.providerAuth)
			ws.runWG.Wait()
		})
	}
}
