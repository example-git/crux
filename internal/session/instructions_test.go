package session

import (
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/stretchr/testify/require"
)

func TestSessionInstructionsPersistWithoutChangingOtherSessions(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	service := NewService(db.New(conn), conn)
	editor := service.(InstructionService)
	first, err := service.Create(t.Context(), "first")
	require.NoError(t, err)
	require.Equal(t, Instructions{Mode: "inherit"}, first.Instructions)
	second, err := service.Create(t.Context(), "second")
	require.NoError(t, err)

	updated, err := editor.SetInstructions(t.Context(), first.ID, Instructions{Mode: "replace", Text: "A literal {{prompt}}\n"})
	require.NoError(t, err)
	require.Equal(t, Instructions{Mode: "replace", Text: "A literal {{prompt}}\n", Revision: 1}, updated.Instructions)
	_, err = editor.SetInstructions(t.Context(), first.ID, Instructions{Mode: "append", Text: "stale"})
	require.ErrorIs(t, err, ErrInstructionsChanged)
	_, err = editor.SetInstructions(t.Context(), first.ID, Instructions{Mode: "invalid", Revision: 1})
	require.ErrorContains(t, err, "instruction mode")

	reopened := NewService(db.New(conn), conn)
	persisted, err := reopened.Get(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, updated.Instructions, persisted.Instructions)
	persisted.Title = "renamed"
	persisted.Instructions = Instructions{}
	persisted, err = reopened.Save(t.Context(), persisted)
	require.NoError(t, err)
	require.Equal(t, updated.Instructions, persisted.Instructions)
	other, err := reopened.Get(t.Context(), second.ID)
	require.NoError(t, err)
	require.Equal(t, Instructions{Mode: "inherit"}, other.Instructions)

	reset, err := editor.SetInstructions(t.Context(), first.ID, Instructions{Mode: "inherit", Text: updated.Instructions.Text, Revision: 1})
	require.NoError(t, err)
	require.EqualValues(t, 2, reset.Instructions.Revision)
	require.Equal(t, "inherit", reset.Instructions.Mode)
	_, err = editor.SetInstructions(t.Context(), first.ID, Instructions{Mode: "replace", Revision: 2})
	require.NoError(t, err)
}
