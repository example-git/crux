package backend

import (
	"context"
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
			require.Nil(t, ws.providerAuth)
			ws.runWG.Wait()
		})
	}
}
