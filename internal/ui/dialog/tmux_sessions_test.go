package dialog

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/tmuxsession"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func newTmuxSessionsTestDialog() *TmuxSessions {
	theme := styles.ThemeForProvider("")
	return NewTmuxSessions(&common.Common{Styles: &theme})
}

func TestTmuxSessionsDialogLoadsFiltersAndSelects(t *testing.T) {
	dialog := newTmuxSessionsTestDialog()
	sessions := []tmuxsession.Session{
		{Socket: tmuxsession.CaptureSocket, ID: "$1", Name: "capture-one", Windows: 1},
		{ID: "$2", Name: "crux-main", Windows: 2, Attached: 1},
	}
	dialog.HandleMsg(tmuxSessionsLoadedMsg{sessions: sessions})
	require.True(t, dialog.loaded)
	require.Len(t, dialog.list.FilteredItems(), 2)

	action, ok := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionAttachTmuxSession)
	require.True(t, ok)
	require.Equal(t, sessions[0], action.Session)

	dialog.HandleMsg(tea.KeyPressMsg{Code: 'm', Text: "m"})
	require.Len(t, dialog.list.FilteredItems(), 1)
	selected, ok := dialog.list.SelectedItem().(*TmuxSessionItem)
	require.True(t, ok)
	require.Equal(t, "$2", selected.session.ID)
}

func TestTmuxSessionsDialogReportsLoadErrorAndEmptySelection(t *testing.T) {
	dialog := newTmuxSessionsTestDialog()
	dialog.HandleMsg(tmuxSessionsLoadedMsg{err: errors.New("tmux unavailable")})
	require.EqualError(t, dialog.loadErr, "tmux unavailable")
	require.Nil(t, dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
}
