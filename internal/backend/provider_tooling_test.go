package backend

import (
	"context"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestProviderToolingRejectsUnadmittedWork(t *testing.T) {
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
			// Nil config makes a mutation beyond admission fail immediately.
			_, err := b.SetProviderToolingInstructions(ctx, id, config.ScopeGlobal, providerregistry.RegistrationOwner{}, config.ToolingInstructionsCrux)
			require.ErrorIs(t, err, expected)
			_, err = b.RemoveProviderToolingInstructions(ctx, id, config.ScopeGlobal, providerregistry.RegistrationOwner{})
			require.ErrorIs(t, err, expected)
			ws.runWG.Wait()
		})
	}
}
