package backend

import (
	"context"

	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/session"
)

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
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return session.Session{}, err
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
	forked, err := ws.Sessions.Create(ctx, source.Title)
	if err != nil {
		return session.Session{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = ws.Sessions.Delete(context.Background(), forked.ID)
		}
	}()
	messageIDs := make(map[string]string, len(messages))
	for _, original := range messages {
		cloned, createErr := ws.Messages.Create(ctx, forked.ID, message.CreateMessageParams{
			Role: original.Role, Parts: original.Clone().Parts, Model: original.Model,
			Provider: original.Provider, IsSummaryMessage: original.IsSummaryMessage,
			PreserveParts: true,
		})
		if createErr != nil {
			return session.Session{}, createErr
		}
		messageIDs[original.ID] = cloned.ID
	}
	forked.PromptTokens = source.PromptTokens
	forked.CompletionTokens = source.CompletionTokens
	forked.EstimatedUsage = source.EstimatedUsage
	forked.Cost = source.Cost
	forked.Todos = append([]session.Todo(nil), source.Todos...)
	forked.SummaryMessageID = messageIDs[source.SummaryMessageID]
	forked, err = ws.Sessions.Save(ctx, forked)
	if err != nil {
		return session.Session{}, err
	}
	if err := ws.Sessions.SetPlanState(ctx, forked.ID, source.Mode, source.Plan); err != nil {
		return session.Session{}, err
	}
	forked, err = ws.Sessions.Get(ctx, forked.ID)
	if err != nil {
		return session.Session{}, err
	}
	committed = true
	return forked, nil
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
	if err := ws.Sessions.SetMode(ctx, sessionID, mode); err != nil {
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
