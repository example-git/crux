package localaddon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/example-git/crux/internal/proto"
)

func TestCodexThreadTurnsProjectsNativeHistory(t *testing.T) {
	turns, err := codexThreadTurns([]proto.Message{
		{
			ID: "user-1", Role: proto.User, SubmissionID: "desktop-message", CreatedAt: 100, UpdatedAt: 100,
			Parts: []proto.ContentPart{
				proto.TextContent{Text: "inspect this", TextElements: []any{map[string]any{"placeholder": "<note>"}}},
				proto.ImageURLContent{URL: "data:image/png;base64,AA==", Detail: "high"},
				proto.SkillContent{Name: "fixture", Path: "/workspace/fixture/SKILL.md", Content: []byte("skill")},
				proto.Finish{Reason: "stop"},
			},
		},
		{
			ID: "assistant-1", Role: proto.Assistant, CreatedAt: 101, UpdatedAt: 125,
			Parts: []proto.ContentPart{
				proto.ReasoningContent{Thinking: "checked the native state"},
				proto.TextContent{Text: "done"},
				proto.Finish{Reason: proto.FinishReasonEndTurn},
			},
		},
	}, false)
	require.NoError(t, err)
	require.Equal(t, []any{map[string]any{
		"id": "user-1", "itemsView": "full", "status": "completed", "error": nil,
		"startedAt": int64(100), "completedAt": int64(125), "durationMs": int64(25),
		"items": []any{
			map[string]any{
				"type": "userMessage", "id": "user-1", "clientId": "desktop-message",
				"content": []any{
					map[string]any{"type": "text", "text": "inspect this", "textElements": []any{map[string]any{"placeholder": "<note>"}}},
					map[string]any{"type": "image", "url": "data:image/png;base64,AA==", "detail": "high"},
					map[string]any{"type": "skill", "name": "fixture", "path": "/workspace/fixture/SKILL.md"},
				},
			},
			map[string]any{"type": "reasoning", "id": "assistant-1-reasoning", "summary": []string{}, "content": []string{"checked the native state"}},
			map[string]any{"type": "agentMessage", "id": "assistant-1", "text": "done"},
		},
	}}, turns)
}

func TestCodexThreadTurnsCoalescesAssistantStepsByItemID(t *testing.T) {
	turns, err := codexThreadTurns([]proto.Message{
		{ID: "turn-1", ItemID: "turn-1", Role: proto.User, CreatedAt: 100, UpdatedAt: 100, Parts: []proto.ContentPart{proto.TextContent{Text: "inspect"}}},
		{ID: "assistant-step-1", ItemID: "assistant-item", Role: proto.Assistant, CreatedAt: 101, UpdatedAt: 110, Parts: []proto.ContentPart{proto.ReasoningContent{Thinking: "first"}, proto.TextContent{Text: "hello "}}},
		{ID: "assistant-step-2", ItemID: "assistant-item", Role: proto.Assistant, CreatedAt: 111, UpdatedAt: 120, Parts: []proto.ContentPart{proto.ReasoningContent{Thinking: "second"}, proto.TextContent{Text: "world"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
	}, false)
	require.NoError(t, err)
	require.Len(t, turns, 1)
	items := turns[0].(map[string]any)["items"].([]any)
	require.Equal(t, []any{
		map[string]any{"type": "userMessage", "id": "turn-1", "clientId": nil, "content": []any{map[string]any{"type": "text", "text": "inspect", "textElements": []any{}}}},
		map[string]any{"type": "reasoning", "id": "assistant-item-reasoning", "summary": []string{}, "content": []string{"first", "second"}},
		map[string]any{"type": "agentMessage", "id": "assistant-item", "text": "hello world"},
	}, items)
}

func TestCodexThreadTurnsGroupsSteeringMessagesByRunID(t *testing.T) {
	turns, err := codexThreadTurns([]proto.Message{
		{ID: "turn-1", RunID: "run-1", Role: proto.User, CreatedAt: 100, UpdatedAt: 100, Parts: []proto.ContentPart{proto.TextContent{Text: "inspect"}}},
		{ID: "assistant-step-1", RunID: "run-1", ItemID: "assistant-item", Role: proto.Assistant, CreatedAt: 101, UpdatedAt: 110, Parts: []proto.ContentPart{proto.ReasoningContent{Thinking: "first"}, proto.TextContent{Text: "hello "}}},
		{ID: "steer-1", RunID: "run-1", Role: proto.User, SubmissionID: "desktop-steer", CreatedAt: 111, UpdatedAt: 111, Parts: []proto.ContentPart{proto.TextContent{Text: "redirect"}}},
		{ID: "assistant-step-2", RunID: "run-1", ItemID: "assistant-item", Role: proto.Assistant, CreatedAt: 112, UpdatedAt: 120, Parts: []proto.ContentPart{proto.ReasoningContent{Thinking: "second"}, proto.TextContent{Text: "world"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
	}, false)
	require.NoError(t, err)
	require.Len(t, turns, 1)
	turn := turns[0].(map[string]any)
	require.Equal(t, "turn-1", turn["id"])
	require.Equal(t, "completed", turn["status"])
	items := turn["items"].([]any)
	require.Len(t, items, 4)
	require.Equal(t, "turn-1", items[0].(map[string]any)["id"])
	require.Equal(t, "assistant-item-reasoning", items[1].(map[string]any)["id"])
	require.Equal(t, "assistant-item", items[2].(map[string]any)["id"])
	require.Equal(t, "hello world", items[2].(map[string]any)["text"])
	require.Equal(t, "steer-1", items[3].(map[string]any)["id"])
	require.Equal(t, "desktop-steer", items[3].(map[string]any)["clientId"])
}

func TestCodexThreadTurnsGroupsInterleavedRunIDs(t *testing.T) {
	turns, err := codexThreadTurns([]proto.Message{
		{ID: "turn-a", RunID: "run-a", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "first"}}},
		{ID: "turn-b", RunID: "run-b", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "second"}}},
		{ID: "steer-a", RunID: "run-a", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "redirect first"}}},
		{ID: "assistant-b", RunID: "run-b", Role: proto.Assistant, Parts: []proto.ContentPart{proto.TextContent{Text: "second done"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
		{ID: "assistant-a", RunID: "run-a", Role: proto.Assistant, Parts: []proto.ContentPart{proto.TextContent{Text: "first done"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
	}, false)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	first := turns[0].(map[string]any)
	second := turns[1].(map[string]any)
	require.Equal(t, "turn-a", first["id"])
	require.Equal(t, "turn-b", second["id"])
	firstItems := first["items"].([]any)
	require.Equal(t, []string{"turn-a", "steer-a", "assistant-a"}, []string{
		firstItems[0].(map[string]any)["id"].(string),
		firstItems[1].(map[string]any)["id"].(string),
		firstItems[2].(map[string]any)["id"].(string),
	})
	secondItems := second["items"].([]any)
	require.Equal(t, []string{"turn-b", "assistant-b"}, []string{
		secondItems[0].(map[string]any)["id"].(string),
		secondItems[1].(map[string]any)["id"].(string),
	})
}

func TestCodexThreadTurnsPreservesMixedLegacyAndRunIDHistory(t *testing.T) {
	turns, err := codexThreadTurns([]proto.Message{
		{ID: "legacy-1", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "legacy first"}}},
		{ID: "legacy-assistant-1", Role: proto.Assistant, Parts: []proto.ContentPart{proto.TextContent{Text: "legacy done"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
		{ID: "native-1", RunID: "run-1", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "native"}}},
		{ID: "native-assistant-1", RunID: "run-1", Role: proto.Assistant, Parts: []proto.ContentPart{proto.TextContent{Text: "native done"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
		{ID: "legacy-2", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "legacy second"}}},
		{ID: "legacy-assistant-2", Role: proto.Assistant, Parts: []proto.ContentPart{proto.TextContent{Text: "legacy done again"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
	}, false)
	require.NoError(t, err)
	require.Len(t, turns, 3)
	require.Equal(t, "legacy-1", turns[0].(map[string]any)["id"])
	require.Equal(t, "native-1", turns[1].(map[string]any)["id"])
	require.Equal(t, "legacy-2", turns[2].(map[string]any)["id"])
	require.Len(t, turns[0].(map[string]any)["items"], 2)
	require.Len(t, turns[1].(map[string]any)["items"], 2)
	require.Len(t, turns[2].(map[string]any)["items"], 2)
}

func TestCodexThreadTurnsRejectsUntaggedUserDuringTaggedRun(t *testing.T) {
	_, err := codexThreadTurns([]proto.Message{
		{ID: "turn-a", RunID: "run-a", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "first"}}},
		{ID: "turn-b", RunID: "run-b", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "second"}}},
		{ID: "assistant-b", RunID: "run-b", Role: proto.Assistant, Parts: []proto.ContentPart{proto.Finish{Reason: proto.FinishReasonEndTurn}}},
		{ID: "legacy-steer", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "redirect"}}},
	}, false)
	require.ErrorContains(t, err, `without a RunID while run "run-a" is active`)
}

func TestCodexThreadTurnsKeepsInterleavedLegacyAssistantWithLegacyTurn(t *testing.T) {
	turns, err := codexThreadTurns([]proto.Message{
		{ID: "legacy-1", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "legacy"}}},
		{ID: "native-1", RunID: "run-1", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "native"}}},
		{ID: "legacy-assistant", Role: proto.Assistant, Parts: []proto.ContentPart{proto.TextContent{Text: "legacy done"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
		{ID: "native-assistant", RunID: "run-1", Role: proto.Assistant, Parts: []proto.ContentPart{proto.TextContent{Text: "native done"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
	}, false)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	legacyItems := turns[0].(map[string]any)["items"].([]any)
	nativeItems := turns[1].(map[string]any)["items"].([]any)
	require.Equal(t, "legacy-assistant", legacyItems[1].(map[string]any)["id"])
	require.Equal(t, "native-assistant", nativeItems[1].(map[string]any)["id"])
}

func TestCodexThreadTurnsRejectsUnknownTaggedAssistant(t *testing.T) {
	_, err := codexThreadTurns([]proto.Message{
		{ID: "turn-a", RunID: "run-a", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "first"}}},
		{ID: "assistant-b", RunID: "run-b", Role: proto.Assistant, Parts: []proto.ContentPart{proto.Finish{Reason: proto.FinishReasonEndTurn}}},
	}, false)
	require.ErrorContains(t, err, `without a user turn for RunID "run-b"`)
}

func TestCodexThreadTurnsExcludesPersistedUserShellCommands(t *testing.T) {
	turns, err := codexThreadTurns([]proto.Message{
		{ID: "user-1", Role: proto.User, CreatedAt: 100, UpdatedAt: 100, Parts: []proto.ContentPart{proto.TextContent{Text: "inspect this"}}},
		{ID: "shell-1", Role: proto.User, CreatedAt: 101, UpdatedAt: 102, Parts: []proto.ContentPart{proto.ShellCommand{Command: "pwd", Output: "/workspace\n", ExitCode: 0}, proto.Finish{Reason: "stop"}}},
		{ID: "assistant-1", Role: proto.Assistant, CreatedAt: 103, UpdatedAt: 125, Parts: []proto.ContentPart{proto.TextContent{Text: "done"}, proto.Finish{Reason: proto.FinishReasonEndTurn}}},
	}, false)
	require.NoError(t, err)
	require.Len(t, turns, 1)
	turn := turns[0].(map[string]any)
	require.Equal(t, "user-1", turn["id"])
	require.Len(t, turn["items"], 2)
}

func TestCodexThreadTurnsPreservesUserWithOnlyCompletionMarker(t *testing.T) {
	message := proto.Message{ID: "user-1", Role: proto.User, Parts: []proto.ContentPart{proto.Finish{Reason: "stop"}}}
	require.True(t, codexEffectiveUserMessage(message))
	turns, err := codexThreadTurns([]proto.Message{message}, true)
	require.NoError(t, err)
	require.Len(t, turns, 1)
	turn := turns[0].(map[string]any)
	require.Equal(t, "inProgress", turn["status"])
	require.Equal(t, []any{map[string]any{"type": "userMessage", "id": "user-1", "clientId": nil, "content": []any{}}}, turn["items"])
}

func TestCodexThreadTurnsPreservesActiveStatus(t *testing.T) {
	turns, err := codexThreadTurns([]proto.Message{{
		ID: "user-1", Role: proto.User, CreatedAt: 100, UpdatedAt: 100,
		Parts: []proto.ContentPart{proto.TextContent{Text: "continue"}, proto.Finish{Reason: "stop"}},
	}}, true)
	require.NoError(t, err)
	turn := turns[0].(map[string]any)
	require.Equal(t, "inProgress", turn["status"])
	require.Nil(t, turn["completedAt"])
	require.Nil(t, turn["durationMs"])
}

func TestCodexThreadTurnsRejectsUnmappedNativeTools(t *testing.T) {
	_, err := codexThreadTurns([]proto.Message{
		{ID: "user-1", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "run"}}},
		{ID: "tool-1", Role: proto.Tool, Parts: []proto.ContentPart{proto.ToolResult{ToolCallID: "call-1", Name: "bash", Content: "ok"}}},
	}, false)
	require.ErrorContains(t, err, "without a faithful Codex item mapping")
}

func TestCodexThreadTurnsPreservesCanceledStatus(t *testing.T) {
	turns, err := codexThreadTurns([]proto.Message{
		{ID: "user-1", Role: proto.User, CreatedAt: 100, UpdatedAt: 100, Parts: []proto.ContentPart{proto.TextContent{Text: "stop"}}},
		{ID: "assistant-1", Role: proto.Assistant, UpdatedAt: 120, Parts: []proto.ContentPart{proto.Finish{Reason: proto.FinishReasonCanceled}}},
	}, false)
	require.NoError(t, err)
	require.Equal(t, "interrupted", turns[0].(map[string]any)["status"])
}

func TestCodexThreadTurnsRejectsAssistantToolCalls(t *testing.T) {
	_, err := codexThreadTurns([]proto.Message{
		{ID: "user-1", Role: proto.User, Parts: []proto.ContentPart{proto.TextContent{Text: "run"}}},
		{ID: "assistant-1", Role: proto.Assistant, Parts: []proto.ContentPart{proto.ToolCall{ID: "call-1", Name: "shell"}}},
	}, false)
	require.ErrorContains(t, err, "cannot project native assistant message")
}

func TestCodexThreadTurnsItemsViews(t *testing.T) {
	full := []any{map[string]any{
		"id": "turn-1", "itemsView": "full", "status": "completed", "error": nil,
		"items": []any{
			map[string]any{"id": "user-1", "type": "userMessage"},
			map[string]any{"id": "reasoning-1", "type": "reasoning"},
			map[string]any{"id": "agent-1", "type": "agentMessage"},
			map[string]any{"id": "agent-2", "type": "agentMessage"},
		},
	}}

	require.NoError(t, applyCodexThreadTurnsItemsView(full, "summary"))
	turn := full[0].(map[string]any)
	require.Equal(t, "summary", turn["itemsView"])
	require.Equal(t, []any{
		map[string]any{"id": "user-1", "type": "userMessage"},
		map[string]any{"id": "agent-2", "type": "agentMessage"},
	}, turn["items"])

	require.NoError(t, applyCodexThreadTurnsItemsView(full, "notLoaded"))
	require.Equal(t, "notLoaded", turn["itemsView"])
	require.Empty(t, turn["items"])
	require.EqualError(t, applyCodexThreadTurnsItemsView(full, "unknown"), "invalid itemsView: unknown")
}

func TestPaginateCodexThreadTurnsSupportsBidirectionalCursors(t *testing.T) {
	turns := []any{
		map[string]any{"id": "turn-1"},
		map[string]any{"id": "turn-2"},
		map[string]any{"id": "turn-3"},
	}
	limit := uint32(2)
	page, next, backwards, err := paginateCodexThreadTurns(turns, nil, &limit, "desc")
	require.NoError(t, err)
	require.Equal(t, []any{turns[2], turns[1]}, page)
	require.NotNil(t, next)
	require.NotNil(t, backwards)
	require.JSONEq(t, `{"turnId":"turn-2","includeAnchor":false}`, *next)
	require.JSONEq(t, `{"turnId":"turn-3","includeAnchor":true}`, *backwards)

	older, olderNext, olderBackwards, err := paginateCodexThreadTurns(turns, next, &limit, "desc")
	require.NoError(t, err)
	require.Equal(t, []any{turns[0]}, older)
	require.Nil(t, olderNext)
	require.JSONEq(t, `{"turnId":"turn-1","includeAnchor":true}`, *olderBackwards)

	newer, newerNext, newerBackwards, err := paginateCodexThreadTurns(turns, olderBackwards, &limit, "asc")
	require.NoError(t, err)
	require.Equal(t, []any{turns[0], turns[1]}, newer)
	require.NotNil(t, newerNext)
	require.JSONEq(t, `{"turnId":"turn-1","includeAnchor":true}`, *newerBackwards)

	zero := uint32(0)
	one, _, _, err := paginateCodexThreadTurns(turns, nil, &zero, "desc")
	require.NoError(t, err)
	require.Equal(t, []any{turns[2]}, one)

	invalid := "not-json"
	_, _, _, err = paginateCodexThreadTurns(turns, &invalid, nil, "desc")
	require.EqualError(t, err, "invalid cursor: not-json")
	missing := `{"turnId":"missing","includeAnchor":false}`
	_, _, _, err = paginateCodexThreadTurns(turns, &missing, nil, "desc")
	require.EqualError(t, err, "invalid cursor: anchor turn is no longer present")

	empty, emptyNext, emptyBackwards, err := paginateCodexThreadTurns(nil, &invalid, nil, "desc")
	require.NoError(t, err)
	require.Empty(t, empty)
	require.Nil(t, emptyNext)
	require.Nil(t, emptyBackwards)
}
