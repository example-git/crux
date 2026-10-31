package server

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSessionModelsAreIncludedInSessionEvents(t *testing.T) {
	models := json.RawMessage(`{"large":{"model":{"provider":"original","model":"main"}}}`)
	current := session.Session{ID: "goal-session", Models: models, ModelRevision: 7, Goal: session.Goal{
		Objective: "finish the work",
		Status:    session.GoalStatusActive,
	}}
	encoded, err := json.Marshal(sessionToProto(current))
	require.NoError(t, err)
	var wire proto.Session
	require.NoError(t, json.Unmarshal(encoded, &wire))
	require.JSONEq(t, string(models), string(wire.Models))
}
