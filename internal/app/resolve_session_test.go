package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

// mockSessionService is a minimal mock of session.Service for testing resolveSession.
type mockSessionService struct {
	sessions []session.Session
	created  []session.Session
}

func (m *mockSessionService) Subscribe(context.Context) <-chan pubsub.Event[session.Session] {
	return make(chan pubsub.Event[session.Session])
}

func (m *mockSessionService) Create(_ context.Context, title string) (session.Session, error) {
	s := session.Session{ID: "new-session-id", Title: title}
	m.created = append(m.created, s)
	return s, nil
}

func (m *mockSessionService) CreateFork(context.Context, string, string) (session.Session, error) {
	return session.Session{}, nil
}

func (m *mockSessionService) CreateTitleSession(context.Context, string) (session.Session, error) {
	return session.Session{}, nil
}

func (m *mockSessionService) CreateTaskSession(context.Context, string, string, string) (session.Session, error) {
	return session.Session{}, nil
}

func (m *mockSessionService) Get(_ context.Context, id string) (session.Session, error) {
	for _, s := range m.sessions {
		if s.ID == id {
			return s, nil
		}
	}
	return session.Session{}, sql.ErrNoRows
}

func (m *mockSessionService) GetLast(_ context.Context) (session.Session, error) {
	if len(m.sessions) > 0 {
		return m.sessions[0], nil
	}
	return session.Session{}, sql.ErrNoRows
}

func (m *mockSessionService) List(context.Context) ([]session.Session, error) {
	return m.sessions, nil
}

func (m *mockSessionService) ListArchived(context.Context) ([]session.Session, error) {
	return nil, nil
}

func (m *mockSessionService) Subtree(context.Context, string) ([]session.Session, error) {
	return nil, nil
}

func (m *mockSessionService) Archive(context.Context, string) ([]session.Session, error) {
	return nil, nil
}

func (m *mockSessionService) Unarchive(context.Context, string) (session.Session, error) {
	return session.Session{}, nil
}

func (m *mockSessionService) Save(_ context.Context, s session.Session) (session.Session, error) {
	return s, nil
}

func (m *mockSessionService) SetMode(context.Context, string, session.Mode) error {
	return nil
}

func (m *mockSessionService) SetAnalysisEffort(context.Context, string, string) error {
	return nil
}

func (m *mockSessionService) SetPlanState(context.Context, string, session.Mode, string) error {
	return nil
}

func (m *mockSessionService) SetGoal(_ context.Context, sessionID, objective string) (session.Session, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return session.Session{}, fmt.Errorf("goal objective is required")
	}
	for index := range m.sessions {
		if m.sessions[index].ID != sessionID {
			continue
		}
		generation := m.sessions[index].Goal.Generation + 1
		if generation < 1 {
			generation = 1
		}
		m.sessions[index].Goal = session.Goal{
			Objective:  objective,
			Status:     session.GoalStatusActive,
			Generation: generation,
		}
		return m.sessions[index], nil
	}
	return session.Session{}, sql.ErrNoRows
}

func (m *mockSessionService) SetGoalStatus(_ context.Context, sessionID string, generation int64, status session.GoalStatus, goalError string) (session.Session, error) {
	for index := range m.sessions {
		if m.sessions[index].ID != sessionID {
			continue
		}
		goal := &m.sessions[index].Goal
		if goal.Objective == "" || goal.Generation != generation {
			return session.Session{}, session.ErrGoalChanged
		}
		goal.Status = status
		goal.Error = strings.TrimSpace(goalError)
		return m.sessions[index], nil
	}
	return session.Session{}, session.ErrGoalChanged
}

func (m *mockSessionService) StopGoal(ctx context.Context, sessionID string, generation int64) (session.Session, error) {
	updated, err := m.SetGoalStatus(ctx, sessionID, generation, session.GoalStatusPaused, "")
	if err != nil {
		return session.Session{}, err
	}
	for index := range m.sessions {
		if m.sessions[index].ID == sessionID {
			m.sessions[index].Goal.Generation++
			return m.sessions[index], nil
		}
	}
	return updated, nil
}

func (m *mockSessionService) ClearGoal(_ context.Context, sessionID string) (session.Session, error) {
	for index := range m.sessions {
		if m.sessions[index].ID != sessionID {
			continue
		}
		generation := m.sessions[index].Goal.Generation + 1
		if generation < 1 {
			generation = 1
		}
		m.sessions[index].Goal = session.Goal{Generation: generation}
		return m.sessions[index], nil
	}
	return session.Session{}, sql.ErrNoRows
}

func (m *mockSessionService) UpdateTitleAndCost(context.Context, string, string, float64) error {
	return nil
}

func (m *mockSessionService) PublishCreated(session.Session) {}

func (m *mockSessionService) PublishCompaction(context.Context, string, bool) error {
	return nil
}

func (m *mockSessionService) PublishRollback(ctx context.Context, sessionID string) (session.Session, error) {
	return m.Get(ctx, sessionID)
}

func (m *mockSessionService) Rename(context.Context, string, string) error {
	return nil
}

func (m *mockSessionService) Delete(context.Context, string) error {
	return nil
}

func (m *mockSessionService) DeleteSubtree(context.Context, string) ([]session.Session, error) {
	return nil, nil
}

func (m *mockSessionService) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return fmt.Sprintf("%s$$%s", messageID, toolCallID)
}

func (m *mockSessionService) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (m *mockSessionService) IsAgentToolSession(sessionID string) bool {
	_, _, ok := m.ParseAgentToolSessionID(sessionID)
	return ok
}

func newTestApp(sessions session.Service) *App {
	return &App{Sessions: sessions}
}

func TestResolveSession_NewSession(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	sess, err := app.resolveSession(t.Context(), "", false)
	require.NoError(t, err)
	require.Equal(t, "new-session-id", sess.ID)
	require.Len(t, mock.created, 1)
}

func TestResolveSession_ContinueByID(t *testing.T) {
	mock := &mockSessionService{
		sessions: []session.Session{
			{ID: "existing-id", Title: "Old session"},
		},
	}
	app := newTestApp(mock)

	sess, err := app.resolveSession(t.Context(), "existing-id", false)
	require.NoError(t, err)
	require.Equal(t, "existing-id", sess.ID)
	require.Equal(t, "Old session", sess.Title)
	require.Empty(t, mock.created)
}

func TestResolveSession_ContinueByID_NotFound(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	_, err := app.resolveSession(t.Context(), "nonexistent", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "session not found")
}

func TestResolveSession_ContinueByID_ChildSession(t *testing.T) {
	mock := &mockSessionService{
		sessions: []session.Session{
			{ID: "child-id", ParentSessionID: "parent-id", Title: "Child session"},
		},
	}
	app := newTestApp(mock)

	_, err := app.resolveSession(t.Context(), "child-id", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot continue a child session")
}

func TestResolveSession_ContinueByID_AgentToolSession(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	_, err := app.resolveSession(t.Context(), "msg123$$tool456", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot continue an agent tool session")
}

func TestResolveSession_Last(t *testing.T) {
	mock := &mockSessionService{
		sessions: []session.Session{
			{ID: "most-recent", Title: "Latest session"},
			{ID: "older", Title: "Older session"},
		},
	}
	app := newTestApp(mock)

	sess, err := app.resolveSession(t.Context(), "", true)
	require.NoError(t, err)
	require.Equal(t, "most-recent", sess.ID)
	require.Empty(t, mock.created)
}

func TestResolveSession_Last_NoSessions(t *testing.T) {
	mock := &mockSessionService{}
	app := newTestApp(mock)

	_, err := app.resolveSession(t.Context(), "", true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no sessions found")
}
