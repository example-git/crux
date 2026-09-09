package backend

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRuntimeControlRejectsUnadmittedWork(t *testing.T) {
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
			_, err := b.RuntimeControlState(ctx, id, config.ScopeGlobal, config.RuntimeControlTarget{})
			require.ErrorIs(t, err, expected)
			_, err = b.SetRuntimeControl(ctx, id, config.ScopeGlobal, config.RuntimeControlTarget{}, json.RawMessage(`false`))
			require.ErrorIs(t, err, expected)
			_, err = b.RemoveRuntimeControl(ctx, id, config.ScopeGlobal, config.RuntimeControlTarget{})
			require.ErrorIs(t, err, expected)
			ws.runWG.Wait()
		})
	}
}
