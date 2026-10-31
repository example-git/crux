package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/pubsub"
)

var ErrModelsChanged = errors.New("session model selection changed")

type modelIdentity struct {
	Large *struct {
		Model struct {
			Provider        string `json:"provider"`
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoning_effort"`
		} `json:"model"`
	} `json:"large"`
}

func sessionModelsJSON(models json.RawMessage) (string, modelIdentity, error) {
	data := strings.TrimSpace(string(models))
	var identity modelIdentity
	if !json.Valid([]byte(data)) || !strings.HasPrefix(data, "{") {
		return "", identity, errors.New("session model selection must be a JSON object")
	}
	if err := json.Unmarshal([]byte(data), &identity); err != nil {
		return "", identity, fmt.Errorf("invalid session model selection: %w", err)
	}
	return data, identity, nil
}

func applyModelIdentity(current *Session, identity modelIdentity) {
	if identity.Large != nil {
		current.Model = identity.Large.Model.Model
		current.Provider = identity.Large.Model.Provider
		current.AnalysisEffort = identity.Large.Model.ReasoningEffort
	}
}

func (s *service) createWithModels(ctx context.Context, params db.CreateSessionParams, models json.RawMessage) (Session, error) {
	var data string
	var identity modelIdentity
	if len(models) > 0 {
		var err error
		data, identity, err = sessionModelsJSON(models)
		if err != nil {
			return Session{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	created, err := s.q.WithTx(tx).CreateSession(ctx, params)
	if err != nil {
		return Session{}, err
	}
	current := s.fromDBItem(created)
	if data != "" {
		if _, err := tx.ExecContext(ctx, "INSERT INTO session_model_selections(session_id, models, revision) VALUES (?, ?, 1)", current.ID, data); err != nil {
			return Session{}, err
		}
		current.Models = json.RawMessage(data)
		current.ModelRevision = 1
		applyModelIdentity(&current, identity)
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET model = ?, provider = ?, analysis_effort = ? WHERE id = ?", current.Model, current.Provider, current.AnalysisEffort, current.ID); err != nil {
			return Session{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Session{}, err
	}
	s.Publish(pubsub.CreatedEvent, current)
	return current, nil
}

func (s *service) SetModels(ctx context.Context, sessionID string, revision int64, models json.RawMessage) (Session, error) {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	return s.setModels(ctx, sessionID, revision, models)
}

func (s *service) setModels(ctx context.Context, sessionID string, revision int64, models json.RawMessage) (Session, error) {
	if revision < 0 {
		return Session{}, errors.New("session model revision must not be negative")
	}
	data, identity, err := sessionModelsJSON(models)
	if err != nil {
		return Session{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
INSERT INTO session_model_selections(session_id, models, revision)
SELECT ?, ?, 1
WHERE ? = 0 OR EXISTS (SELECT 1 FROM session_model_selections WHERE session_id = ? AND revision = ?)
ON CONFLICT(session_id) DO UPDATE SET models = excluded.models, revision = session_model_selections.revision + 1
WHERE session_model_selections.revision = ?`, sessionID, data, revision, sessionID, revision, revision)
	if err != nil {
		return Session{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Session{}, err
	}
	if changed != 1 {
		return Session{}, ErrModelsChanged
	}
	if identity.Large != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET model = ?, provider = ?, analysis_effort = ? WHERE id = ?", identity.Large.Model.Model, identity.Large.Model.Provider, identity.Large.Model.ReasoningEffort, sessionID); err != nil {
			return Session{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Session{}, err
	}
	current, err := s.Get(ctx, sessionID)
	if err != nil {
		return Session{}, err
	}
	s.Publish(pubsub.UpdatedEvent, current)
	return current, nil
}

func (s *service) withModels(ctx context.Context, current Session) (Session, error) {
	var data string
	err := s.db.QueryRowContext(ctx, "SELECT models, revision FROM session_model_selections WHERE session_id = ?", current.ID).Scan(&data, &current.ModelRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return current, nil
	}
	if err != nil {
		return Session{}, err
	}
	models, identity, err := sessionModelsJSON(json.RawMessage(data))
	if err != nil {
		return Session{}, err
	}
	current.Models = json.RawMessage(models)
	applyModelIdentity(&current, identity)
	return current, nil
}
