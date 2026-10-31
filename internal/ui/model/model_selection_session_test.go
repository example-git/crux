package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

type sessionModelSelectionWorkspace struct {
	*testWorkspace
	sessions map[string]session.Session
	updated  []string
}

func (w *sessionModelSelectionWorkspace) GetSession(_ context.Context, id string) (session.Session, error) {
	current, ok := w.sessions[id]
	if !ok {
		return session.Session{}, errors.New("session not found")
	}
	return current, nil
}

func (w *sessionModelSelectionWorkspace) SetSessionModels(_ context.Context, id string, revision int64, models config.AgentModelState) (session.Session, error) {
	current := w.sessions[id]
	if current.ModelRevision != revision {
		return session.Session{}, session.ErrModelsChanged
	}
	data, err := json.Marshal(models)
	if err != nil {
		return session.Session{}, err
	}
	current.Models, current.ModelRevision = data, revision+1
	w.sessions[id] = current
	w.updated = append(w.updated, id)
	return current, nil
}

func TestModelSelectionUpdatesOnlyOriginatingSessionInEveryGoalState(t *testing.T) {
	for _, status := range []session.GoalStatus{"", session.GoalStatusActive, session.GoalStatusPaused, session.GoalStatusBlocked} {
		t.Run(string(status), func(t *testing.T) {
			ui, base, action, _ := modelSelectionTestUI(t, true)
			before, err := json.Marshal(base.Config().AgentModelState())
			require.NoError(t, err)
			first := session.Session{ID: "first", Models: before, ModelRevision: 1, Goal: session.Goal{Objective: "translation", Status: status, Generation: 1}}
			second := session.Session{ID: "second", Models: before, ModelRevision: 1, Goal: session.Goal{Objective: "categorization", Status: session.GoalStatusActive, Generation: 2}}
			ws := &sessionModelSelectionWorkspace{testWorkspace: base, sessions: map[string]session.Session{"first": first, "second": second}}
			ui.com.Workspace = ws
			ui.session = &first
			cmd := ui.enqueueModelSelection(action, "", "")
			require.NotNil(t, cmd)
			ui.session = &second
			result, ok := cmd().(modelSelectionCompletedMsg)
			require.True(t, ok)
			require.NoError(t, result.err)
			require.Equal(t, second, ws.sessions["second"])
			require.Equal(t, []string{"first"}, ws.updated)
			require.Equal(t, first.Goal, ws.sessions["first"].Goal)
			require.NotEqual(t, before, ws.sessions["first"].Models)
			require.EqualValues(t, 2, ws.sessions["first"].ModelRevision)
			_ = ui.completeModelSelection(result)
			require.Equal(t, "second", ui.session.ID)
			require.Equal(t, 0, base.updateAgentCalls)
		})
	}
}
