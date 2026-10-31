package app

import (
	"context"
	"encoding/json"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/session"
)

func (app *App) SetSessionModels(ctx context.Context, sessionID string, revision int64, models config.AgentModelState) (session.Session, error) {
	var current session.Session
	err := app.config.WithRuntimeSnapshot(func(snapshot config.RuntimeSnapshot) error {
		if _, err := snapshot.WithAgentModels(models); err != nil {
			return err
		}
		data, err := json.Marshal(models)
		if err != nil {
			return err
		}
		current, err = app.Sessions.SetModels(ctx, sessionID, revision, data)
		return err
	})
	return current, err
}
