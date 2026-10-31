package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

type serverRollbackHistory struct {
	history.Service
}

func (*serverRollbackHistory) RewindCheckpoints(context.Context, string, []string, bool) error {
	return nil
}

func TestSessionRollbackRouteCommitsThroughWorkspaceBackend(t *testing.T) {
	harness := newE2EHarness(t)
	directory := t.TempDir()
	connection, err := db.Connect(t.Context(), directory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(directory)) })
	queries := db.New(connection)
	sessions := session.NewService(queries, connection)
	messages := message.NewService(queries, message.WithCompactionStore(connection, sessions))
	harness.app.Sessions = sessions
	harness.app.Messages = messages
	harness.app.History = &serverRollbackHistory{}
	current, err := sessions.Create(t.Context(), "rollback")
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "first"}}})
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "first response"}}})
	require.NoError(t, err)
	secondUser, err := messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "second"}}})
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "second response"}}})
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{"message_id": secondUser.ID})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.httpSrv.URL+"/v1/workspaces/"+harness.workspace.ID+"/sessions/"+current.ID+"/rollback", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")

	response, err := harness.httpSrv.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var rollback proto.SessionRollbackResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&rollback))
	require.Len(t, rollback.Messages, 2)
	require.Equal(t, int64(2), rollback.Session.MessageCount)

	stored, err := messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	require.Len(t, stored, 2)
}
