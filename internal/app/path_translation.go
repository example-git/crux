package app

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/proto"
)

func (app *App) pathTranslationScope(ctx context.Context, snapshot config.RuntimeSnapshot, scope pathtranslate.Scope, attachment *proto.WorkspaceAttachment) error {
	if _, err := scope.Key(); err != nil {
		return err
	}
	if app.PathTranslations == nil {
		return errors.New("path translation store is unavailable")
	}
	if err := snapshot.RuntimeRevocation(); err != nil {
		return err
	}
	if authority := snapshot.RemoteAuthority(); authority != nil {
		if attachment == nil || attachment.Mode != "client" || attachment.Revision != authority.Revision || attachment.Digest != authority.Digest {
			return config.ErrRemoteRuntimeRevision
		}
	}
	_, err := app.Sessions.Get(ctx, scope.SessionID)
	return err
}

func (app *App) LoadPathTranslation(ctx context.Context, scope pathtranslate.Scope, attachment *proto.WorkspaceAttachment) (pathtranslate.Policy, error) {
	var policy pathtranslate.Policy
	err := app.config.WithRuntimeSnapshot(func(snapshot config.RuntimeSnapshot) error {
		if err := app.pathTranslationScope(ctx, snapshot, scope, attachment); err != nil {
			return err
		}
		var err error
		policy, _, err = app.PathTranslations.Load(ctx, scope)
		return err
	})
	return policy, err
}

func (app *App) SavePathTranslation(ctx context.Context, policy pathtranslate.Policy, attachment *proto.WorkspaceAttachment) (pathtranslate.Policy, error) {
	var saved pathtranslate.Policy
	err := app.config.WithRuntimeSnapshot(func(snapshot config.RuntimeSnapshot) error {
		if err := app.pathTranslationScope(ctx, snapshot, policy.Scope, attachment); err != nil {
			return err
		}
		var err error
		saved, err = app.PathTranslations.Save(ctx, policy)
		return err
	})
	return saved, err
}
