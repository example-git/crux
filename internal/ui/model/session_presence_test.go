package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

func TestSessionPresenceDelayedCommandCannotReplaceNewSelection(t *testing.T) {
	ui, ws := newAuthoritySessionUI(t, "receiver")
	older := ui.reportCurrentSession("A")
	newer := ui.reportCurrentSession("B")
	require.Empty(t, ws.presence, "scheduling must perform no IO")
	ws.io = true
	newer()
	older()
	ws.io = false
	require.Equal(t, []string{"B"}, ws.presence)
	// New Session is a real selection: its clear must supersede commands
	// already returned by an accepted load, even if those commands run last.
	older = ui.reportCurrentSession("A")
	commands := ui.newSession()().(tea.BatchMsg)
	require.Nil(t, ui.session)
	ws.io = true
	commands[2]() // reportCurrentSession("") from newSession's batch.
	older()
	ws.io = false
	require.Equal(t, []string{"B", ""}, ws.presence)
}

func TestSessionPresenceCurrentCommandSurvivesSameObjectRecreation(t *testing.T) {
	ui, ws := newAuthoritySessionUI(t, "old-receiver")
	older := ui.reportCurrentSession("old-selection")
	ws.id = "new-receiver"
	current := ui.reportCurrentSession("restored-selection")
	ws.io = true
	older()
	current()
	ws.io = false
	require.Equal(t, []string{"restored-selection"}, ws.presence)
}
