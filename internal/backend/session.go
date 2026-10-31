package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/session"
)

var ErrSessionActiveWriter = errors.New("session has an active writer")

type sessionActiveWriterError struct {
	id string
}

func (e sessionActiveWriterError) Error() string {
	return fmt.Sprintf("thread %s already has an active writer", e.id)
}

func (e sessionActiveWriterError) Is(target error) bool {
	return target == ErrSessionActiveWriter
}

// CreateSession creates a new session in the given workspace.
func (b *Backend) CreateSession(ctx context.Context, workspaceID, title string) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}

	return ws.Sessions.Create(ctx, title)
}

// ForkSession creates a new top-level session containing an independent copy
// of the source session's persisted messages and metadata.
func (b *Backend) ForkSession(ctx context.Context, workspaceID, sessionID string) (session.Session, error) {
	return b.forkSession(ctx, workspaceID, sessionID, false)
}

func (b *Backend) ForkSessionWithOrigin(ctx context.Context, workspaceID, sessionID string) (session.Session, error) {
	return b.forkSession(ctx, workspaceID, sessionID, true)
}

func (b *Backend) forkSession(ctx context.Context, workspaceID, sessionID string, recordOrigin bool) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}
	if coordinator := ws.CurrentAgentCoordinator(); coordinator != nil && coordinator.IsSessionBusy(sessionID) {
		return session.Session{}, sessionActiveWriterError{id: sessionID}
	}
	if err := ws.Messages.FlushAll(ctx); err != nil {
		return session.Session{}, err
	}
	source, err := ws.Sessions.Get(ctx, sessionID)
	if err != nil {
		return session.Session{}, err
	}
	messages, err := ws.Messages.List(ctx, sessionID)
	if err != nil {
		return session.Session{}, err
	}
	return ws.Messages.CloneSession(ctx, message.CloneSessionParams{
		Source: source, Messages: messages, RecordOrigin: recordOrigin,
	})
}

// GetSession retrieves a session by workspace and session ID.
func (b *Backend) GetSession(ctx context.Context, workspaceID, sessionID string) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}

	return ws.Sessions.Get(ctx, sessionID)
}

// ListSessions returns all sessions in the given workspace.
func (b *Backend) ListSessions(ctx context.Context, workspaceID string) ([]session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	return ws.Sessions.List(ctx)
}

func (b *Backend) ListArchivedSessions(ctx context.Context, workspaceID string) ([]session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	return ws.Sessions.ListArchived(ctx)
}

func (b *Backend) ArchiveSession(ctx context.Context, workspaceID, sessionID string) ([]session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	subtree, err := ws.Sessions.Subtree(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if coordinator := ws.CurrentAgentCoordinator(); coordinator != nil {
		for _, current := range subtree {
			if coordinator.IsSessionBusy(current.ID) {
				return nil, sessionActiveWriterError{id: current.ID}
			}
		}
	}
	archived, err := ws.Sessions.Archive(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for _, current := range archived {
		ws.ResetAgentSession(current.ID)
	}
	return archived, nil
}

func (b *Backend) UnarchiveSession(ctx context.Context, workspaceID, sessionID string) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}
	if coordinator := ws.CurrentAgentCoordinator(); coordinator != nil && coordinator.IsSessionBusy(sessionID) {
		return session.Session{}, sessionActiveWriterError{id: sessionID}
	}
	return ws.Sessions.Unarchive(ctx, sessionID)
}

// GetAgentSession returns session metadata with the agent's busy
// status.
func (b *Backend) GetAgentSession(ctx context.Context, workspaceID, sessionID string) (proto.AgentSession, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.AgentSession{}, err
	}

	se, err := ws.Sessions.Get(ctx, sessionID)
	if err != nil {
		return proto.AgentSession{}, err
	}

	var isSessionBusy bool
	if coordinator := ws.CurrentAgentCoordinator(); coordinator != nil {
		isSessionBusy = coordinator.IsSessionBusy(sessionID)
	}

	return proto.AgentSession{
		Session: proto.Session{
			ID:    se.ID,
			Title: se.Title,
			Mode:  string(se.Mode),
			Plan:  se.Plan,
		},
		IsBusy: isSessionBusy,
	}, nil
}

// ListSessionMessages returns all messages for a session.
func (b *Backend) ListSessionMessages(ctx context.Context, workspaceID, sessionID string) ([]message.Message, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	// Drain debounced updates so HTTP clients (and the TUI on session
	// switch) observe the latest in-memory state rather than racing the
	// debounce timer in message.Service.
	if err := ws.Messages.FlushAll(ctx); err != nil {
		return nil, err
	}
	return ws.Messages.List(ctx, sessionID)
}

// ListSessionHistory returns the history items for a session.
func (b *Backend) ListSessionHistory(ctx context.Context, workspaceID, sessionID string) (any, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	return ws.History.ListLatestCheckpointFiles(ctx, sessionID)
}

// SaveSession updates a session in the given workspace.
func (b *Backend) SaveSession(ctx context.Context, workspaceID string, sess session.Session) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}

	return ws.Sessions.Save(ctx, sess)
}

func (b *Backend) SetSessionMode(ctx context.Context, workspaceID, sessionID string, mode session.Mode) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}
	if mode == session.ModeDefault {
		current, err := ws.Sessions.Get(ctx, sessionID)
		if err != nil {
			return session.Session{}, err
		}
		if current.Mode.IsPlan() {
			if coordinator := ws.CurrentAgentCoordinator(); coordinator != nil {
				coordinator.Cancel(sessionID)
			}
		}
	}
	if err := ws.Sessions.SetMode(ctx, sessionID, mode); err != nil {
		return session.Session{}, err
	}
	return ws.Sessions.Get(ctx, sessionID)
}

func (b *Backend) SetSessionAnalysisEffort(ctx context.Context, workspaceID, sessionID, effort string) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}
	if err := ws.Sessions.SetAnalysisEffort(ctx, sessionID, effort); err != nil {
		return session.Session{}, err
	}
	return ws.Sessions.Get(ctx, sessionID)
}

func (b *Backend) SetSessionGoal(ctx context.Context, workspaceID, sessionID, objective string) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}
	return ws.Sessions.SetGoal(ctx, sessionID, objective)
}

func (b *Backend) SetSessionGoalStatus(ctx context.Context, workspaceID, sessionID string, generation int64, status session.GoalStatus, goalError string) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}
	return ws.Sessions.SetGoalStatus(ctx, sessionID, generation, status, goalError)
}

func (b *Backend) StopSessionGoal(ctx context.Context, workspaceID, sessionID string, generation int64) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}
	updated, err := ws.Sessions.StopGoal(ctx, sessionID, generation)
	if err != nil {
		return session.Session{}, err
	}
	if coordinator := ws.CurrentAgentCoordinator(); coordinator != nil {
		coordinator.Cancel(sessionID)
	}
	return updated, nil
}

func (b *Backend) ClearSessionGoal(ctx context.Context, workspaceID, sessionID string) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}
	return ws.Sessions.ClearGoal(ctx, sessionID)
}

func (b *Backend) RenameSession(ctx context.Context, workspaceID, sessionID, title string) (session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
	}
	if err := ws.Sessions.Rename(ctx, sessionID, title); err != nil {
		return session.Session{}, err
	}
	return ws.Sessions.Get(ctx, sessionID)
}

// DeleteSession deletes a session from the given workspace.
func (b *Backend) DeleteSession(ctx context.Context, workspaceID, sessionID string) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	if err := ws.Sessions.Delete(ctx, sessionID); err != nil {
		return err
	}
	ws.ResetAgentSession(sessionID)
	return nil
}

func (b *Backend) DeleteSessionSubtree(ctx context.Context, workspaceID, sessionID string) ([]session.Session, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	subtree, err := ws.Sessions.Subtree(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if coordinator := ws.CurrentAgentCoordinator(); coordinator != nil {
		for _, current := range subtree {
			if coordinator.IsSessionBusy(current.ID) {
				return nil, sessionActiveWriterError{id: current.ID}
			}
		}
	}
	deleted, err := ws.Sessions.DeleteSubtree(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for _, current := range deleted {
		ws.ResetAgentSession(current.ID)
	}
	return deleted, nil
}

// ListUserMessages returns user-role messages for a session.
func (b *Backend) ListUserMessages(ctx context.Context, workspaceID, sessionID string) ([]message.Message, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	return ws.Messages.ListUserMessages(ctx, sessionID)
}

// ListAllUserMessages returns all user-role messages across sessions.
func (b *Backend) ListAllUserMessages(ctx context.Context, workspaceID string) ([]message.Message, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	return ws.Messages.ListAllUserMessages(ctx)
}
