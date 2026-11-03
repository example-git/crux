package model

import (
	"context"
	"testing"

	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

type pathTranslationMenuWorkspace struct {
	*goalWorkspace
	scopes []pathtranslate.Scope
}

func (w *pathTranslationMenuWorkspace) LoadPathTranslation(_ context.Context, scope pathtranslate.Scope) (pathtranslate.Policy, error) {
	w.scopes = append(w.scopes, scope)
	return pathtranslate.Policy{Scope: scope}, nil
}

func TestPathTranslationMenuCreatesSessionAndKeepsDialogSessionBound(t *testing.T) {
	ws := &pathTranslationMenuWorkspace{goalWorkspace: newGoalWorkspace()}
	theme := styles.ThemeForProvider("")
	m := &UI{com: &common.Common{Workspace: ws, Styles: &theme}, dialog: dialog.NewOverlay()}
	cmd := m.openPathTranslationDialog()
	require.NotNil(t, cmd)
	require.Zero(t, ws.created)
	require.Nil(t, m.openPathTranslationDialog())
	created := cmd().(pathTranslationSessionMsg)
	require.NoError(t, created.err)
	require.Equal(t, 1, ws.created)
	require.Equal(t, m.com.Workspace, created.source)
	m.session = &created.session
	m.pathTranslationOpening = false
	cmd = m.openPathTranslationDialog()
	require.NotNil(t, cmd)
	first := m.dialog.Dialog(dialog.PathTranslationID).(*dialog.PathTranslation)
	first.HandleMsg(cmd())
	require.Equal(t, created.session.ID, first.SessionID())
	require.Equal(t, []pathtranslate.Scope{{Kind: "session", SessionID: created.session.ID}}, ws.scopes)
	require.Nil(t, m.openPathTranslationDialog())
	m.session = &session.Session{ID: "another-session"}
	cmd = m.openPathTranslationDialog()
	require.NotNil(t, cmd)
	second := m.dialog.Dialog(dialog.PathTranslationID).(*dialog.PathTranslation)
	require.NotSame(t, first, second)
	second.HandleMsg(cmd())
	require.Equal(t, "another-session", second.SessionID())
	require.Equal(t, pathtranslate.Scope{Kind: "session", SessionID: "another-session"}, ws.scopes[1])
	require.Equal(t, 1, ws.created)
}
