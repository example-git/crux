package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func clientSettingsRequest(t *testing.T, value string) ClientSettingsWriteRequest {
	t.Helper()
	var request ClientSettingsWriteRequest
	require.NoError(t, json.Unmarshal([]byte(value), &request))
	return request
}

func TestClientSettingsWritesNativeStateAndRetainedRead(t *testing.T) {
	store, root, base := authenticationBasisStore(t, func(root string, _ map[string]string) {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "data", "crux.json"), []byte(`{"remote_control":{"legacy":true},"features":{"remote_control":true,"unrelated":true},"client_settings":{"other_client":{"kept":true}}}`), 0o600))
	})
	models := store.Config().Models
	request := clientSettingsRequest(t, `{"edits":[{"keyPath":"remote_control","value":null,"mergeStrategy":"replace"},{"keyPath":"features.remote_control","value":null,"mergeStrategy":"replace"},{"keyPath":"desktop.selected-avatar-id","value":"codex","mergeStrategy":"replace"},{"keyPath":"desktop.workspace","value":{"collapsed":true,"width":320},"mergeStrategy":"replace"}]}`)
	result, err := store.WriteClientSettings(t.Context(), request)
	require.NoError(t, err)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedRoot, "data", "crux.json"), result.FilePath)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, result.Version)
	view, err := store.ReadConfigView(t.Context(), ScopeGlobal)
	require.NoError(t, err)
	require.NoError(t, view.Validate())
	expected := map[string]any{"selected-avatar-id": "codex", "workspace": map[string]any{"collapsed": true, "width": float64(320)}}
	require.Equal(t, expected, view.Desktop)
	require.Equal(t, models, store.Config().Models)
	for key := range expected {
		require.Equal(t, ConfigReadOrigin{Kind: "user", Path: filepath.Join(root, "data", "crux.json"), Version: result.Version}, view.DesktopOrigins[key])
	}
	data, err := os.ReadFile(result.FilePath)
	require.NoError(t, err)
	var saved map[string]any
	require.NoError(t, json.Unmarshal(data, &saved))
	require.NotContains(t, saved, "remote_control")
	require.Equal(t, map[string]any{"unrelated": true}, saved["features"])
	require.Equal(t, map[string]any{"kept": true}, saved["client_settings"].(map[string]any)["other_client"])
	view.Desktop["workspace"].(map[string]any)["width"] = float64(999)
	again, err := store.ReadConfigView(t.Context(), ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, float64(320), again.Desktop["workspace"].(map[string]any)["width"])
	request = clientSettingsRequest(t, `{"edits":[{"keyPath":"desktop.workspace","value":{"width":640},"mergeStrategy":"upsert"}]}`)
	request.ExpectedVersion = &result.Version
	updated, err := store.WriteClientSettings(t.Context(), request)
	require.NoError(t, err)
	require.NotEqual(t, result.Version, updated.Version)
	view, err = store.ReadConfigView(t.Context(), ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"collapsed": true, "width": float64(640)}, view.Desktop["workspace"])
	request = clientSettingsRequest(t, `{"edits":[{"keyPath":"desktop.workspace.width","value":null,"mergeStrategy":"replace"}]}`)
	_, err = store.WriteClientSettings(t.Context(), request)
	require.NoError(t, err)
	view, err = store.ReadConfigView(t.Context(), ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"collapsed": true}, view.Desktop["workspace"])
	restarted, err := LoadIsolated(root, filepath.Join(root, "workspace"), false, base)
	require.NoError(t, err)
	restored, err := restarted.ReadConfigView(t.Context(), ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, view, restored)
	require.Equal(t, models, restarted.Config().Models)
}

func TestClientSettingsRejectsAtomicInvalidWritesAndStaleVersions(t *testing.T) {
	store, _, _ := authenticationBasisStore(t, nil)
	initial := clientSettingsRequest(t, `{"edits":[{"keyPath":"desktop.workspace","value":{"width":320},"mergeStrategy":"replace"}]}`)
	result, err := store.WriteClientSettings(t.Context(), initial)
	require.NoError(t, err)
	before, err := os.ReadFile(result.FilePath)
	require.NoError(t, err)
	for _, value := range []string{
		`{"edits":[{"keyPath":"desktop.workspace","value":{"width":999},"mergeStrategy":"replace"},{"keyPath":"model","value":"different","mergeStrategy":"replace"}]}`,
		`{"edits":[{"keyPath":"desktop.workspace","value":false,"mergeStrategy":"replace"},{"keyPath":"desktop.workspace.width","value":999,"mergeStrategy":"upsert"}]}`,
		`{"edits":[{"keyPath":"desktop.workspace","value":999,"mergeStrategy":"invalid"}]}`,
		`{"edits":[{"keyPath":"remote_control","value":true,"mergeStrategy":"replace"}]}`,
		`{"edits":[{"keyPath":"desktop..width","value":999,"mergeStrategy":"replace"}]}`,
	} {
		_, err := store.WriteClientSettings(t.Context(), clientSettingsRequest(t, value))
		require.ErrorIs(t, err, ErrClientSettingsValidation, value)
		after, err := os.ReadFile(result.FilePath)
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
	request := initial
	stale := "sha256:stale"
	request.ExpectedVersion = &stale
	_, err = store.WriteClientSettings(t.Context(), request)
	require.ErrorIs(t, err, ErrClientSettingsVersionConflict)
	outside := filepath.Join(t.TempDir(), "must-not-write.json")
	request = initial
	request.FilePath = &outside
	_, err = store.WriteClientSettings(t.Context(), request)
	require.ErrorIs(t, err, ErrClientSettingsReadonly)
	require.NoFileExists(t, outside)
	after, err := os.ReadFile(result.FilePath)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestClientSettingsConcurrentVersionFence(t *testing.T) {
	store, root, base := authenticationBasisStore(t, nil)
	initial := clientSettingsRequest(t, `{"edits":[{"keyPath":"desktop.value","value":0,"mergeStrategy":"replace"}]}`)
	result, err := store.WriteClientSettings(t.Context(), initial)
	require.NoError(t, err)
	other, err := LoadIsolated(root, filepath.Join(root, "workspace"), false, base)
	require.NoError(t, err)
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, writer := range []*ConfigStore{store, other} {
		request := initial
		request.Edits = []ClientSettingsEdit{{KeyPath: "desktop.value", Value: json.RawMessage{'1' + byte(index)}, MergeStrategy: "replace"}}
		request.ExpectedVersion = &result.Version
		go func() {
			<-start
			_, err := writer.WriteClientSettings(t.Context(), request)
			results <- err
		}()
	}
	close(start)
	first, second := <-results, <-results
	require.True(t, first == nil && errors.Is(second, ErrClientSettingsVersionConflict) || second == nil && errors.Is(first, ErrClientSettingsVersionConflict), "%v / %v", first, second)
}
