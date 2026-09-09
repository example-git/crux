package model

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/chat"
)

type PreviewStoredMessage struct {
	Number int    `json:"number"`
	ID     string `json:"id"`
	Role   string `json:"role"`
	ItemID string `json:"itemId"`
}

// NewPreviewSession reads an explicitly selected database using the normal
// decoders. It performs no migrations, workspace startup, or message writes.
func NewPreviewSession(ctx context.Context, dbPath, workingDir, sessionID string) (*Preview, error) {
	conn, err := db.ConnectReadOnly(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return nil, err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	rows, err := conn.QueryContext(ctx, "PRAGMA table_info(sessions)")
	if err != nil {
		return nil, err
	}
	hasUnseen := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			rows.Close()
			return nil, err
		}
		if name == "unseen_local_tokens" {
			hasUnseen = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	q := db.New(previewSessionReader{DB: conn, legacy: !hasUnseen})
	current, err := session.NewService(q, conn).Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	messages, err := message.NewService(q).List(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("decode session messages: %w", err)
	}
	files, err := history.NewService(q, conn).ListBySession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session file history: %w", err)
	}
	p, err := NewPreview()
	if err != nil {
		return nil, err
	}
	p.imported = true
	if !hasUnseen {
		p.schemaNote = "Older session schema: unseen_local_tokens defaults to 0; database unchanged"
	}
	p.workingDir = workingDir
	p.data.Session = current
	p.data.Messages = make([]*message.Message, len(messages))
	for i := range messages {
		p.data.Messages[i] = &messages[i]
	}
	p.data.MCP = nil
	p.data.LSP = nil
	p.data.Usage = nil
	p.data.Files = nil
	// Runtime-only service state is unavailable in a persisted session. The
	// provider/model controls stay explicitly dummy; source identities remain in
	// the unchanged message records rather than inventing a historical catalog.
	p.initialize(p.data.Models[0])
	ws := p.ui.com.Workspace.(*previewWorkspace)
	ws.history = files
	p.data.Files, err = loadSessionFiles(ctx, ws, sessionID)
	if err != nil {
		return nil, err
	}
	p.base = clonePreviewValue(reflect.ValueOf(p.data)).Interface().(*PreviewData)
	p.ui = nil
	p.itemsKey = ""
	return p, nil
}
func (p *Preview) FixtureData() *PreviewData {
	return clonePreviewValue(reflect.ValueOf(p.base)).Interface().(*PreviewData)
}
func (w *previewWorkspace) ListSessionHistory(context.Context, string) ([]history.File, error) {
	return w.history, nil
}
func clonePreviewValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	out := reflect.New(v.Type()).Elem()
	out.Set(v)
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			out.Set(reflect.New(v.Type().Elem()))
			out.Elem().Set(clonePreviewValue(v.Elem()))
		}
	case reflect.Interface:
		if !v.IsNil() {
			out.Set(clonePreviewValue(v.Elem()))
		}
	case reflect.Slice:
		if !v.IsNil() {
			out = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			for i := 0; i < v.Len(); i++ {
				out.Index(i).Set(clonePreviewValue(v.Index(i)))
			}
		}
	case reflect.Map:
		if !v.IsNil() {
			out = reflect.MakeMap(v.Type())
			it := v.MapRange()
			for it.Next() {
				out.SetMapIndex(it.Key(), clonePreviewValue(it.Value()))
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if out.Field(i).CanSet() && v.Field(i).CanInterface() {
				out.Field(i).Set(clonePreviewValue(v.Field(i)))
			}
		}
	}
	return out
}
func (p *Preview) importedItems(o PreviewOptions) []chat.MessageItem {
	messages := p.data.Messages
	results := chat.BuildToolResultMap(messages)
	items := []chat.MessageItem{}
	p.storedMessages = nil
	owners := map[string]string{}
	for i, msg := range messages {
		next := chat.ExtractMessageItems(p.ui.com.Styles, msg, results, p.workingDir)
		row := PreviewStoredMessage{Number: i + 1, ID: msg.ID, Role: string(msg.Role)}
		if len(next) > 0 {
			row.ItemID = next[0].ID()
		}
		for _, item := range next {
			if tool, ok := item.(chat.ToolMessageItem); ok {
				owners[tool.ToolCall().ID] = item.ID()
			}
			if c, ok := item.(chat.Compactable); ok {
				c.SetCompact(o.ToolsCompact)
			}
			if o.ToolsExpanded {
				if c, ok := item.(chat.Expandable); ok {
					c.ToggleExpanded()
				}
			}
		}
		p.storedMessages = append(p.storedMessages, row)
		items = append(items, next...)
	}
	for i, msg := range messages {
		if p.storedMessages[i].ItemID == "" {
			for _, result := range msg.ToolResults() {
				if id := owners[result.ToolCallID]; id != "" {
					p.storedMessages[i].ItemID = id
					break
				}
			}
		}
	}
	return items
}

// Project only the newly introduced counter when opening an older schema.
// All other fields still use the generated query and the normal session decoder.
type previewSessionReader struct {
	*sql.DB
	legacy bool
}

func (r previewSessionReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if r.legacy && strings.HasPrefix(query, "-- name: GetSessionByID :one") {
		query = strings.Replace(query, ", unseen_local_tokens", ", 0 AS unseen_local_tokens", 1)
	}
	return r.DB.QueryRowContext(ctx, query, args...)
}
