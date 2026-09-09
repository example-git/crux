package backend

import (
	"context"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOverrideModelsRejectsUnadmittedWork(t *testing.T) {
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
			// A nil App/config makes an unexpected mutation observable as a panic.
			_, err := b.OverrideModels(ctx, id, config.AgentModelState{})
			require.ErrorIs(t, err, expected)
			ws.runWG.Wait()
		})
	}
}
