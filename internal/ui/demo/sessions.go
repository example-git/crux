package demo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/model"
	"github.com/google/uuid"
)

type sessionSource struct {
	runtimeConfig              *config.Config
	project, database, initial string
	mu                         sync.Mutex
	snapshots                  map[string]*model.Preview
}

func newSessionSource(project, initial string) (*sessionSource, error) {
	s := &sessionSource{initial: initial, snapshots: map[string]*model.Preview{}}
	if project == "" {
		if initial != "" {
			return nil, fmt.Errorf("session requires a project selected at startup")
		}
		return s, nil
	}
	root, err := filepath.Abs(project)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, ".crux", "crux.db")
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("project database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("project database is not a regular file")
	}
	s.project = root
	s.database = path
	conn, err := db.ConnectReadOnly(context.Background(), path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if initial != "" {
		id, err := resolveDemoSessionID(context.Background(), conn, initial)
		if err != nil {
			return nil, fmt.Errorf("initial session in %s: %w", path, err)
		}
		s.initial = id
	}

	return s, nil
}
func (s *sessionSource) preview(id string) (*model.Preview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.snapshots[id]
	if p == nil {
		return nil, fmt.Errorf("unknown imported snapshot; reload the session from the configured source")
	}
	return p, nil
}
func (s *sessionSource) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if len(r.URL.Query()) > 0 {
			http.Error(w, `{"error":"session source is configured by startup flags only"}`, 400)
			return
		}
		result := map[string]any{"enabled": s.database != "", "project": s.project, "initialSessionId": s.initial, "sessions": []any{}}
		if s.database == "" {
			json.NewEncoder(w).Encode(result)
			return
		}
		conn, err := db.ConnectReadOnly(r.Context(), s.database)
		if err != nil {
			http.Error(w, `{"error":"cannot open configured session database"}`, 500)
			return
		}
		defer conn.Close()
		rows, err := conn.QueryContext(r.Context(), "SELECT id, title, COALESCE(parent_session_id,''), message_count, updated_at FROM sessions ORDER BY updated_at DESC")
		if err != nil {
			http.Error(w, `{"error":"cannot read configured session catalog"}`, 500)
			return
		}
		defer rows.Close()
		sessions := []map[string]any{}
		for rows.Next() {
			var id, title, parent string
			var count, updated int64
			if err = rows.Scan(&id, &title, &parent, &count, &updated); err != nil {
				break
			}
			sessions = append(sessions, map[string]any{"id": id, "shortId": session.HashID(id), "title": title, "parentId": parent, "messageCount": count, "updatedAt": updated})
		}
		if err != nil || rows.Err() != nil {
			http.Error(w, `{"error":"cannot decode session catalog"}`, 500)
			return
		}
		result["sessions"] = sessions
		json.NewEncoder(w).Encode(result)
	})
	mux.HandleFunc("POST /api/sessions/load", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, err error) {
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
		if s.database == "" {
			fail(409, fmt.Errorf("no project selected; restart demo with --project"))
			return
		}
		var body struct {
			SessionID string `json:"sessionId"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			fail(400, err)
			return
		}
		if body.SessionID == "" {
			fail(400, fmt.Errorf("sessionId is required"))
			return
		}
		conn, err := db.ConnectReadOnly(r.Context(), s.database)
		if err != nil {
			fail(400, err)
			return
		}
		id, err := resolveDemoSessionID(r.Context(), conn, body.SessionID)
		conn.Close()
		if err != nil {
			fail(400, err)
			return
		}
		body.SessionID = id
		p, err := model.NewPreviewSession(r.Context(), s.database, s.project, body.SessionID)
		if err != nil {
			fail(400, err)
			return
		}
		if s.runtimeConfig != nil {
			if err := p.UseLaunchProviders(s.runtimeConfig); err != nil {
				fail(400, err)
				return
			}
		}
		snapshotID := uuid.NewString()
		s.mu.Lock()
		s.snapshots[snapshotID] = p
		s.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"importId": snapshotID, "sessionId": body.SessionID, "model": p.InitialModel()})
	})
}

func resolveDemoSessionID(ctx context.Context, conn *sql.DB, value string) (string, error) {
	var exact string
	err := conn.QueryRowContext(ctx, "SELECT id FROM sessions WHERE id = ?", value).Scan(&exact)
	if err == nil {
		return exact, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	rows, err := conn.QueryContext(ctx, "SELECT id FROM sessions WHERE parent_session_id IS NULL")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	matches := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		if strings.HasPrefix(session.HashID(id), value) {
			matches = append(matches, id)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("session ID %q is ambiguous (%d matches)", value, len(matches))
	}
	return "", fmt.Errorf("session not found: %s", value)
}
