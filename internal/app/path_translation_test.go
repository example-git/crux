package app

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestPathTranslationSettingsValidateScopeAndPreserveRevisions(t *testing.T) {
	app, _ := newAgentRecoveryTestApp(t)
	dir := t.TempDir()
	conn, err := db.Connect(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dir)) })
	app.PathTranslations = pathtranslate.NewStore(conn)
	app.Sessions = session.NewService(db.New(conn), conn)
	one, err := app.Sessions.Create(t.Context(), "one")
	require.NoError(t, err)
	two, err := app.Sessions.Create(t.Context(), "two")
	require.NoError(t, err)
	provider := pathtranslate.Scope{Kind: "provider", Provider: "available"}
	model := pathtranslate.Scope{Kind: "model", Provider: "available", Model: "model"}
	settings := pathtranslate.Settings{Enabled: true, Targets: []pathtranslate.Target{{Path: "/nonexistent/large protected tree", Recursive: true, CensorImages: true}}}
	for _, scope := range []pathtranslate.Scope{provider, model} {
		_, err := app.SavePathTranslation(t.Context(), pathtranslate.Policy{Scope: scope, Settings: settings}, nil)
		require.ErrorContains(t, err, "only session scope")
	}
	scope := pathtranslate.Scope{Kind: "session", SessionID: one.ID}
	loaded, err := app.LoadPathTranslation(t.Context(), scope, nil)
	require.NoError(t, err)
	require.False(t, loaded.Settings.Enabled)
	loaded.Settings = pathtranslate.Settings{}
	disabled, err := app.SavePathTranslation(t.Context(), loaded, nil)
	require.NoError(t, err)
	effective, found, err := app.PathTranslations.Effective(t.Context(), one.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, disabled, effective)
	effective, found, err = app.PathTranslations.Effective(t.Context(), two.ID)
	require.NoError(t, err)
	require.False(t, found)
	require.False(t, effective.Settings.Enabled)
	removed := disabled
	removed.Settings = pathtranslate.Settings{Inherit: true}
	removed, err = app.SavePathTranslation(t.Context(), removed, nil)
	require.NoError(t, err)
	require.EqualValues(t, disabled.Revision+1, removed.Revision)
	effective, _, err = app.PathTranslations.Effective(t.Context(), one.ID)
	require.NoError(t, err)
	require.False(t, effective.Settings.Enabled)
	_, err = app.SavePathTranslation(t.Context(), disabled, nil)
	require.ErrorContains(t, err, "changed")
	removed.Settings = settings
	recreated, err := app.SavePathTranslation(t.Context(), removed, nil)
	require.NoError(t, err)
	require.EqualValues(t, 3, recreated.Revision)
	_, err = app.SavePathTranslation(t.Context(), disabled, nil)
	require.ErrorContains(t, err, "changed")
	for _, scope := range []pathtranslate.Scope{
		{Kind: "session", SessionID: "missing"},
		{Kind: "provider", Provider: "missing"},
		{Kind: "model", Provider: "available", Model: "missing"},
		{Kind: "project"},
	} {
		_, err := app.SavePathTranslation(t.Context(), pathtranslate.Policy{Scope: scope, Settings: settings}, nil)
		require.Error(t, err)
	}
	var count int
	require.NoError(t, conn.QueryRow(`SELECT count(*) FROM path_translation_names`).Scan(&count))
	require.Zero(t, count)
	data, err := json.Marshal(recreated)
	require.NoError(t, err)
	require.NotContains(t, string(data), "secret")
	require.NotContains(t, string(data), "identity")
}
