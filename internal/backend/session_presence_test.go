package backend

import (
	"testing"
	"time"

	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func numberedSessionPresence(sessionID string, generation uint64) proto.CurrentSession {
	return proto.CurrentSession{SessionID: sessionID, SelectionGeneration: &generation}
}

func TestSessionPresenceReceiverGenerationAndClaimRearm(t *testing.T) {
	for _, pendingResponse := range []bool{false, true} {
		t.Run(map[bool]string{false: "timer-hold", true: "response-hold"}[pendingResponse], func(t *testing.T) {
			b, _ := newTestBackend(t)
			b.SetDetachGrace(time.Hour)
			ws, _ := insertTestWorkspace(t, b, t.TempDir())
			id, other := newClientID(t), newClientID(t)
			require.NoError(t, b.AttachClient(ws.ID, id))
			require.NoError(t, b.AttachClient(ws.ID, other))
			t.Cleanup(func() { _ = b.RetireClient(id); _ = b.RetireClient(other) })
			require.NoError(t, b.SetCurrentSession(ws.ID, id, "legacy"))
			require.ErrorIs(t, b.SetCurrentSessionSelection(ws.ID, id, numberedSessionPresence("invalid", 0)), ErrSessionSelectionInvalid)
			require.NoError(t, b.SetCurrentSessionSelection(ws.ID, id, numberedSessionPresence("B", 2)))
			require.NoError(t, b.SetCurrentSessionSelection(ws.ID, id, numberedSessionPresence("A", 1)))
			require.NoError(t, b.SetCurrentSessionSelection(ws.ID, id, numberedSessionPresence("B", 2)))
			require.ErrorIs(t, b.SetCurrentSessionSelection(ws.ID, id, numberedSessionPresence("conflict", 2)), ErrSessionSelectionConflict)
			require.ErrorIs(t, b.SetCurrentSession(ws.ID, id, "legacy-late"), ErrSessionSelectionConflict)
			require.NoError(t, b.SetCurrentSession(ws.ID, other, "other-legacy"))
			b.DetachClient(ws.ID, id)
			b.mu.Lock()
			if pendingResponse {
				b.pendingResponses[id]++
			}
			b.registerClient(ws, id)
			b.mu.Unlock()
			require.NoError(t, b.AttachClient(ws.ID, id))
			require.NoError(t, b.SetCurrentSessionSelection(ws.ID, id, numberedSessionPresence("A-after-rearm", 1)))
			ws.clientsMu.Lock()
			require.Equal(t, "B", ws.clients[id].currentSessionID)
			require.EqualValues(t, 2, ws.clients[id].currentSessionGeneration)
			require.Equal(t, "other-legacy", ws.clients[other].currentSessionID)
			ws.clientsMu.Unlock()
			require.NoError(t, b.SetCurrentSessionSelection(ws.ID, id, numberedSessionPresence("", 3)))
			require.NoError(t, b.SetCurrentSessionSelection(ws.ID, id, numberedSessionPresence("B", 2)))
			ws.clientsMu.Lock()
			require.Empty(t, ws.clients[id].currentSessionID)
			require.EqualValues(t, 3, ws.clients[id].currentSessionGeneration)
			ws.clientsMu.Unlock()
		})
	}
}
