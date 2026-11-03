package session

import (
	"context"
	"database/sql"
	"errors"

	"github.com/example-git/crux/internal/pubsub"
)

type Instructions struct {
	Mode     string `json:"mode"`
	Text     string `json:"text"`
	Revision int64  `json:"revision"`
}

var ErrInstructionsChanged = errors.New("session instructions changed; reload before saving")

func (i Instructions) Validate() error {
	if i.Mode != "inherit" && i.Mode != "append" && i.Mode != "replace" {
		return errors.New("instruction mode must be inherit, append, or replace")
	}
	if i.Revision < 0 {
		return errors.New("instruction revision must not be negative")
	}
	return nil
}

type InstructionService interface {
	SetInstructions(context.Context, string, Instructions) (Session, error)
}

func (s *service) SetInstructions(ctx context.Context, id string, instructions Instructions) (Session, error) {
	if err := instructions.Validate(); err != nil {
		return Session{}, err
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO session_instructions(session_id, mode, text, revision)
SELECT ?, ?, ?, 1
WHERE ? = 0 OR EXISTS (SELECT 1 FROM session_instructions WHERE session_id = ? AND revision = ?)
ON CONFLICT(session_id) DO UPDATE SET mode = excluded.mode, text = excluded.text, revision = session_instructions.revision + 1
WHERE session_instructions.revision = ?`, id, instructions.Mode, instructions.Text, instructions.Revision, id, instructions.Revision, instructions.Revision)
	if err != nil {
		return Session{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Session{}, err
	}
	if changed != 1 {
		return Session{}, ErrInstructionsChanged
	}
	current, err := s.Get(ctx, id)
	if err != nil {
		return Session{}, err
	}
	s.Publish(pubsub.UpdatedEvent, current)
	return current, nil
}

func (s *service) withInstructions(ctx context.Context, current Session) (Session, error) {
	current.Instructions.Mode = "inherit"
	err := s.db.QueryRowContext(ctx, "SELECT mode, text, revision FROM session_instructions WHERE session_id = ?", current.ID).Scan(&current.Instructions.Mode, &current.Instructions.Text, &current.Instructions.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return current, nil
	}
	return current, err
}
