package localaddon

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

type codexRollbackProtocolClient struct {
	encoder     *json.Encoder
	decoder     *json.Decoder
	inputWriter *io.PipeWriter
	done        <-chan error
}

func newCodexRollbackProtocolClient(t *testing.T, workingDir, clientName string, optOut []string) *codexRollbackProtocolClient {
	t.Helper()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source:     "codex",
		Protocol:   compatibility.ProtocolCodexAppServer,
		Style:      compatibility.ExecutionHeadless,
		WorkingDir: workingDir,
		Prompt:     compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader},
		Session:    compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	client := &codexRollbackProtocolClient{encoder: json.NewEncoder(inputWriter), decoder: json.NewDecoder(outputReader), inputWriter: inputWriter, done: done}
	params := map[string]any{"clientInfo": map[string]any{"name": clientName, "version": "1"}}
	if optOut != nil {
		params["capabilities"] = map[string]any{"optOutNotificationMethods": optOut}
	}
	require.NoError(t, client.encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": params}))
	response := client.decode(t)
	require.Equal(t, float64(1), response["id"])
	return client
}

func (c *codexRollbackProtocolClient) close(t *testing.T) {
	t.Helper()
	require.NoError(t, c.inputWriter.Close())
	require.NoError(t, <-c.done)
}

func (c *codexRollbackProtocolClient) decode(t *testing.T) map[string]any {
	t.Helper()
	var response map[string]any
	require.NoError(t, c.decoder.Decode(&response))
	return response
}

func (c *codexRollbackProtocolClient) request(t *testing.T, id int, method string, params map[string]any) {
	t.Helper()
	require.NoError(t, c.encoder.Encode(map[string]any{"id": id, "method": method, "params": params}))
}

func requireCodexRollbackNotice(t *testing.T, response map[string]any) {
	t.Helper()
	require.Equal(t, "deprecationNotice", response["method"])
	require.Equal(t, map[string]any{"summary": "thread/rollback is deprecated and will be removed soon"}, response["params"])
}

func codexRollbackMessages(sessionID string) []proto.Message {
	return []proto.Message{
		{ID: "system", Role: proto.System, SessionID: sessionID, CreatedAt: 1, UpdatedAt: 1, Parts: []proto.ContentPart{proto.TextContent{Text: "system"}}},
		{ID: "user-1", Role: proto.User, SessionID: sessionID, CreatedAt: 2, UpdatedAt: 2, Parts: []proto.ContentPart{proto.TextContent{Text: "first"}}},
		{ID: "assistant-1", Role: proto.Assistant, SessionID: sessionID, CreatedAt: 3, UpdatedAt: 3, Parts: []proto.ContentPart{proto.TextContent{Text: "first response"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
		{ID: "user-2", Role: proto.User, SessionID: sessionID, CreatedAt: 4, UpdatedAt: 4, Parts: []proto.ContentPart{proto.TextContent{Text: "second"}}},
		{ID: "assistant-2", Role: proto.Assistant, SessionID: sessionID, CreatedAt: 5, UpdatedAt: 5, Parts: []proto.ContentPart{proto.TextContent{Text: "second response"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
		{ID: "shell", Role: proto.User, SessionID: sessionID, CreatedAt: 6, UpdatedAt: 6, Parts: []proto.ContentPart{proto.ShellCommand{Command: "pwd", Output: "/workspace\n", ExitCode: 0}}},
		{ID: "summary", Role: proto.User, SessionID: sessionID, CreatedAt: 7, UpdatedAt: 7, IsSummaryMessage: true, Parts: []proto.ContentPart{proto.TextContent{Text: "summary"}}},
	}
}

func TestCodexAppServerRollbackPersistsEffectiveTurnBoundary(t *testing.T) {
	const sessionID = "rollback-history"
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{}, 25*time.Millisecond, true, []nativeSessionSeed{{id: sessionID, messages: codexRollbackMessages(sessionID)}})
	threadID := codexUUIDv7(workingDir, &proto.Session{ID: sessionID, CreatedAt: 1})
	client := newCodexRollbackProtocolClient(t, workingDir, "desktop", []string{"deprecationNotice"})
	defer client.close(t)

	client.request(t, 2, "thread/rollback", map[string]any{"threadId": threadID, "numTurns": 1})
	requireCodexRollbackNotice(t, client.decode(t))
	response := client.decode(t)
	require.Equal(t, float64(2), response["id"])
	turns := response["result"].(map[string]any)["thread"].(map[string]any)["turns"].([]any)
	require.Len(t, turns, 1)
	require.Equal(t, "user-1", turns[0].(map[string]any)["id"])

	client.request(t, 3, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
	response = client.decode(t)
	turns = response["result"].(map[string]any)["thread"].(map[string]any)["turns"].([]any)
	require.Len(t, turns, 1)
	require.Equal(t, "user-1", turns[0].(map[string]any)["id"])

	client.request(t, 4, "thread/rollback", map[string]any{"threadId": threadID, "numTurns": 99})
	requireCodexRollbackNotice(t, client.decode(t))
	response = client.decode(t)
	require.Equal(t, float64(4), response["id"])
	require.Empty(t, response["result"].(map[string]any)["thread"].(map[string]any)["turns"])

	client.request(t, 5, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
	response = client.decode(t)
	require.Empty(t, response["result"].(map[string]any)["thread"].(map[string]any)["turns"])
}

func TestCodexAppServerRollbackNoEffectiveTurnsIsNoOp(t *testing.T) {
	const sessionID = "rollback-no-op"
	messages := []proto.Message{
		{ID: "system", Role: proto.System, SessionID: sessionID, Parts: []proto.ContentPart{proto.TextContent{Text: "system"}}},
		{ID: "shell", Role: proto.User, SessionID: sessionID, Parts: []proto.ContentPart{proto.ShellCommand{Command: "pwd", ExitCode: 0}}},
		{ID: "summary", Role: proto.User, SessionID: sessionID, IsSummaryMessage: true, Parts: []proto.ContentPart{proto.TextContent{Text: "summary"}}},
	}
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{}, 25*time.Millisecond, true, []nativeSessionSeed{{id: sessionID, messages: messages}})
	threadID := codexUUIDv7(workingDir, &proto.Session{ID: sessionID, CreatedAt: 1})
	client := newCodexRollbackProtocolClient(t, workingDir, "desktop", nil)
	defer client.close(t)

	client.request(t, 2, "thread/rollback", map[string]any{"threadId": threadID, "numTurns": 5})
	requireCodexRollbackNotice(t, client.decode(t))
	response := client.decode(t)
	require.Equal(t, float64(2), response["id"])
	require.Empty(t, response["result"].(map[string]any)["thread"].(map[string]any)["turns"])
}

func TestCodexAppServerRollbackValidatesAndProjectsNativeErrors(t *testing.T) {
	const activeSessionID = "rollback-active"
	const emptySessionID = "rollback-empty"
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{}, 25*time.Millisecond, true, []nativeSessionSeed{
		{id: activeSessionID, active: true, messages: codexRollbackMessages(activeSessionID)},
		{id: emptySessionID, messages: []proto.Message{}},
	})
	activeThreadID := codexUUIDv7(workingDir, &proto.Session{ID: activeSessionID, CreatedAt: 1})
	emptyThreadID := codexUUIDv7(workingDir, &proto.Session{ID: emptySessionID, CreatedAt: 1})
	client := newCodexRollbackProtocolClient(t, workingDir, "desktop", nil)

	tests := []struct {
		id      int
		params  map[string]any
		message string
	}{
		{id: 2, params: map[string]any{"threadId": activeThreadID, "numTurns": 0}, message: "numTurns must be >= 1"},
		{id: 3, params: map[string]any{"threadId": activeThreadID, "numTurns": 1}, message: "Cannot rollback while a turn is in progress."},
		{id: 4, params: map[string]any{"threadId": emptyThreadID, "numTurns": 1}, message: "thread rollback requires persisted thread history"},
	}
	for _, test := range tests {
		client.request(t, test.id, "thread/rollback", test.params)
		requireCodexRollbackNotice(t, client.decode(t))
		response := client.decode(t)
		require.Equal(t, float64(test.id), response["id"])
		require.Equal(t, test.message, response["error"].(map[string]any)["message"])
	}
}

func TestCodexAppServerRollbackSuppressesNoticeForExactTUIClient(t *testing.T) {
	const sessionID = "rollback-tui"
	workingDir := nativeAPIFixtureWithSessionSeeds(t, proto.RunComplete{}, 25*time.Millisecond, true, []nativeSessionSeed{{id: sessionID, messages: codexRollbackMessages(sessionID)}})
	threadID := codexUUIDv7(workingDir, &proto.Session{ID: sessionID, CreatedAt: 1})
	client := newCodexRollbackProtocolClient(t, workingDir, "codex-tui", nil)
	defer client.close(t)

	client.request(t, 2, "thread/rollback", map[string]any{"threadId": threadID, "numTurns": 1})
	response := client.decode(t)
	require.Equal(t, float64(2), response["id"])
	require.NotContains(t, response, "method")
}
