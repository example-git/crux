package demo

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/model"
)

func TestPreviewFlagScopedSessionReplay(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	conn, err := db.Connect(ctx, filepath.Join(project, ".crux"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	q := db.New(conn)
	_, err = q.CreateSession(ctx, db.CreateSessionParams{ID: "saved-session", Title: "Real persisted fixture"})
	if err != nil {
		t.Fatal(err)
	}
	messages := message.NewService(q)
	for _, params := range []message.CreateMessageParams{
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Persisted user input"}}},
		{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "saved-call", Name: "bash", Input: `{"command":"echo persisted"}`, Finished: true}, message.Finish{Reason: message.FinishReasonToolUse}}},
		{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "saved-call", Name: "bash", Content: "Persisted tool output"}}},
	} {
		if _, err := messages.Create(ctx, "saved-session", params); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE sessions DROP COLUMN unseen_local_tokens"); err != nil {
		t.Fatal(err)
	}
	handler, err := newHandler(project, session.HashID("saved-session"), false)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/sessions/load", strings.NewReader(`{"sessionId":"saved-session","project":"/elsewhere"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatal("API accepted a filesystem target")
	}
	req = httptest.NewRequestWithContext(t.Context(), "POST", "/api/sessions/load", strings.NewReader(`{"sessionId":"saved-session"}`))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var loaded map[string]string
	json.Unmarshal(w.Body.Bytes(), &loaded)
	options := model.PreviewOptions{ImportID: loaded["importId"], Cols: 180, Rows: 60, Example: "all", Model: "dummy-coder", Scenario: "idle", Modal: "none", Popover: "none", MessageNumber: 3}
	raw, _ := json.Marshal(options)
	req = httptest.NewRequestWithContext(t.Context(), "POST", "/api/preview", strings.NewReader(string(raw)))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var frame model.PreviewFrame
	json.Unmarshal(w.Body.Bytes(), &frame)
	if frame.SchemaNote == "" {
		t.Fatal("legacy schema default was not disclosed")
	}
	if !frame.Imported || len(frame.StoredMessages) != 3 || frame.StoredMessages[2].ItemID == "" || !strings.Contains(frame.Content, "Persisted tool output") {
		t.Fatal("saved tool result did not replay through native item")
	}
	ro, err := db.ConnectReadOnly(ctx, filepath.Join(project, ".crux", "crux.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.ExecContext(ctx, "DELETE FROM messages"); err == nil {
		t.Fatal("read-only connection accepted a write")
	}
}
