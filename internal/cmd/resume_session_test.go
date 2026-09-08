package cmd

import (
	"context"
	"fmt"
	"testing"

	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type resumeSessionWorkspace struct {
	workspace.Workspace
	sessions []session.Session
}

func (w resumeSessionWorkspace) GetSession(_ context.Context, id string) (session.Session, error) {
	for _, saved := range w.sessions {
		if saved.ID == id {
			return saved, nil
		}
	}
	return session.Session{}, fmt.Errorf("session not found: %s", id)
}

func (w resumeSessionWorkspace) ListSessions(context.Context) ([]session.Session, error) {
	return w.sessions, nil
}

func TestResumeWorkspaceSessionID(t *testing.T) {
	saved := session.Session{ID: "acdbf4d0-7a44-43db-b127-f8e36f7c0fc2"}
	ws := resumeSessionWorkspace{sessions: []session.Session{saved}}
	for _, id := range []string{saved.ID, session.HashID(saved.ID), session.HashID(saved.ID)[:7]} {
		t.Run(id, func(t *testing.T) {
			resolved, err := resolveWorkspaceSessionID(t.Context(), ws, id)
			require.NoError(t, err)
			require.Equal(t, saved.ID, resolved.ID)
		})
	}
	_, err := resolveWorkspaceSessionID(t.Context(), ws, "not-a-session")
	require.ErrorContains(t, err, "session not found")

	prefixes := make(map[string]session.Session)
	for index := range 17 {
		candidate := session.Session{ID: fmt.Sprintf("resume-candidate-%d", index)}
		prefix := session.HashID(candidate.ID)[:1]
		if previous, ok := prefixes[prefix]; ok {
			ws.sessions = []session.Session{previous, candidate}
			_, err := resolveWorkspaceSessionID(t.Context(), ws, prefix)
			require.ErrorContains(t, err, "ambiguous")
			return
		}
		prefixes[prefix] = candidate
	}
	t.Fatal("expected two hexadecimal hashes with a shared first character")
}
