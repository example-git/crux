package model

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type queuedSelectionWorkspace struct {
	*testWorkspace
	store            *config.ConfigStore
	entered, release chan struct{}
	writes           atomic.Int32
	fail             bool
	authority        *config.RemoteAuthority
}

func (w *queuedSelectionWorkspace) Config() *config.Config { return w.store.Config() }
func (w *queuedSelectionWorkspace) AcceptedAuthority() *config.RemoteAuthority {
	return w.authority
}

func (w *queuedSelectionWorkspace) ProviderSurfaces() []providerregistry.Surface {
	return config.ProviderSurfaces(w.Config())
}

func (w *queuedSelectionWorkspace) UpdatePreferredModel(scope config.Scope, kind config.SelectedModelType, model config.SelectedModel, owner providerregistry.RegistrationOwner) (config.AgentModelState, error) {
	if w.writes.Add(1) == 1 && w.entered != nil {
		close(w.entered)
		<-w.release
	}
	if w.fail {
		return config.AgentModelState{}, errors.New("synthetic persistence failure")
	}
	return w.store.UpdatePreferredModelForOwner(scope, kind, model, owner)
}

func (w *queuedSelectionWorkspace) UpdateAgentModel(context.Context, config.AgentModelState) error {
	return nil
}

func (w *queuedSelectionWorkspace) PermissionSkipRequests() bool                     { return false }
func (w *queuedSelectionWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo { return nil }

func queuedSelectionFixture(t *testing.T) (*UI, *queuedSelectionWorkspace, dialog.ActionSelectModel, string) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"config", "data", "project"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o700))
	}
	data := []byte(`{"providers":{"queue":{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"synthetic-key","models":[{"id":"first"},{"id":"second"},{"id":"small"}]}},"models":{"large":{"provider":"queue","model":"first"},"small":{"provider":"queue","model":"small"}}}`)
	path := filepath.Join(root, "data", "crux.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": "integrated", "CRUX_DISABLE_AUTO_MEMORY": "true"}
	store, err := config.LoadIsolated(filepath.Join(root, "project"), filepath.Join(root, "workspace-data"), false, env.NewFromMap(values))
	require.NoError(t, err)
	ws := &queuedSelectionWorkspace{testWorkspace: &testWorkspace{}, store: store}
	ui := newTestUI()
	ui.com.Workspace = ws
	ui.dialog = dialog.NewOverlay()
	ui.header = newHeader(ui.com)
	ui.themeKey = styles.ThemeKeyForProvider("queue")
	provider, ok := store.Config().Providers.Get("queue")
	require.True(t, ok)
	owner, ok := store.Config().ProviderOwner("queue")
	require.True(t, ok)
	action := dialog.ActionSelectModel{Provider: provider.ToProvider(), Model: config.SelectedModel{Provider: "queue", Model: "first"}, ModelType: config.SelectedModelTypeLarge, ProviderOwner: owner, ProviderOwnerSet: true}
	return ui, ws, action, path
}

func singleSelectionCompletion(t *testing.T, command tea.Cmd) modelSelectionCompletedMsg {
	t.Helper()
	var results []modelSelectionCompletedMsg
	for _, message := range collectCommandMessages(command) {
		if result, ok := message.(modelSelectionCompletedMsg); ok {
			results = append(results, result)
		}
	}
	require.Len(t, results, 1)
	return results[0]
}

func TestModelSelectionQueueSerializesActualPersistence(t *testing.T) {
	ui, ws, first, path := queuedSelectionFixture(t)
	ws.entered, ws.release = make(chan struct{}), make(chan struct{})
	command := ui.handleSelectModel(first)
	require.Zero(t, ws.writes.Load(), "Update cannot persist")
	done := make(chan modelSelectionCompletedMsg, 1)
	go func() { done <- command().(modelSelectionCompletedMsg) }()
	select {
	case <-ws.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first write did not enter")
	}
	second := first
	second.Model.Model = "second"
	require.Nil(t, ui.handleSelectModel(second), "second command remains queued")
	require.EqualValues(t, 1, ws.writes.Load())
	close(ws.release)
	var completed modelSelectionCompletedMsg
	select {
	case completed = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("first write did not finish")
	}
	require.NoError(t, completed.err)
	_, next := ui.Update(completed)
	require.Zero(t, ui.usageFetchGen, "stale completion must not update current UI")
	result := singleSelectionCompletion(t, next)
	require.NoError(t, result.err)
	require.EqualValues(t, 2, ws.writes.Load())
	require.Equal(t, "second", ws.Config().Models[config.SelectedModelTypeLarge].Model)
	var saved struct {
		Models map[string]config.SelectedModel `json:"models"`
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &saved))
	require.Equal(t, "second", saved.Models["large"].Model)
	_, _ = ui.Update(result)
	require.Empty(t, ui.modelSelectionLanes)
	require.Equal(t, uint64(1), ui.usageFetchGen)
}

func TestModelSelectionQueueReleasesFailureAndRetainsWorkspace(t *testing.T) {
	ui, ws, first, _ := queuedSelectionFixture(t)
	ws.fail = true
	command := ui.handleSelectModel(first)
	second := first
	second.Model.Model = "second"
	require.Nil(t, ui.handleSelectModel(second))
	failed := singleSelectionCompletion(t, command)
	require.Error(t, failed.err)
	replacement, _, _, _ := queuedSelectionFixture(t)
	ui.com.Workspace = replacement.com.Workspace
	_, next := ui.Update(failed)
	ws.fail = false
	result := singleSelectionCompletion(t, next)
	require.NoError(t, result.err)
	require.Equal(t, "second", ws.Config().Models[config.SelectedModelTypeLarge].Model)
	require.Equal(t, "first", ui.com.Config().Models[config.SelectedModelTypeLarge].Model)
	_, _ = ui.Update(result)
	require.Empty(t, ui.modelSelectionLanes)
	require.Zero(t, ui.usageFetchGen)
	// Duplicate old completion cannot restart or adopt either operation.
	_, _ = ui.Update(failed)
	require.EqualValues(t, 2, ws.writes.Load())
}

func TestModelSelectionQueueCapturesValuesAndRejectsStaleControl(t *testing.T) {
	ui, ws, first, _ := queuedSelectionFixture(t)
	temp := 0.25
	first.Model.Temperature = &temp
	first.Model.ProviderOptions = map[string]any{"nested": map[string]any{"value": "original"}}
	command := ui.handleSelectModel(first)
	temp = 0.9
	first.Model.ProviderOptions["nested"].(map[string]any)["value"] = "changed"
	result := singleSelectionCompletion(t, command)
	require.NoError(t, result.err)
	selected := ws.Config().Models[config.SelectedModelTypeLarge]
	require.Equal(t, 0.25, *selected.Temperature)
	require.Equal(t, "original", selected.ProviderOptions["nested"].(map[string]any)["value"])
	_, _ = ui.Update(result)
	second := first
	second.Model.Model = "second"
	command = ui.handleSelectModel(second)
	require.Nil(t, ui.queueModelControl("reasoning", "high"))
	result = singleSelectionCompletion(t, command)
	require.NoError(t, result.err)
	_, next := ui.Update(result)
	stale := singleSelectionCompletion(t, next)
	require.ErrorContains(t, stale.err, "model changed before its control update")
	require.Equal(t, "second", ws.Config().Models[config.SelectedModelTypeLarge].Model)
	require.NotEqual(t, "high", ws.Config().Models[config.SelectedModelTypeLarge].ReasoningEffort)
}

func TestModelSelectionQueueControlsAndSessionRestore(t *testing.T) {
	ui, ws, _, _ := queuedSelectionFixture(t)
	command := ui.queueModelControl("thinking", "")
	require.Zero(t, ws.writes.Load())
	require.Nil(t, ui.queueModelControl("reasoning", "high"))
	thinking := singleSelectionCompletion(t, command)
	require.NoError(t, thinking.err)
	_, next := ui.Update(thinking)
	reasoning := singleSelectionCompletion(t, next)
	require.NoError(t, reasoning.err)
	current := ws.Config().Models[config.SelectedModelTypeLarge]
	require.True(t, current.Think)
	require.Equal(t, "high", current.ReasoningEffort)
	_, _ = ui.Update(reasoning)
	writes := ws.writes.Load()
	command = ui.restoreModelFromSession([]message.Message{{Role: message.Assistant, Provider: "queue", Model: "second"}})
	require.NotNil(t, command)
	require.Equal(t, writes, ws.writes.Load(), "session restoration cannot persist in Update")
	restored := singleSelectionCompletion(t, command)
	require.NoError(t, restored.err)
	require.Equal(t, "second", ws.Config().Models[config.SelectedModelTypeLarge].Model)
	require.Equal(t, "small", ws.Config().Models[config.SelectedModelTypeSmall].Model)
	_, _ = ui.Update(restored)
	require.Empty(t, ui.modelSelectionLanes)

	ws.authority = &config.RemoteAuthority{Mode: "client", Principal: "client-principal", Revision: 2, Digest: strings.Repeat("a", 64)}
	writes = ws.writes.Load()
	command = ui.restoreModelFromSession([]message.Message{{Role: message.Assistant, Provider: "queue", Model: "first"}})
	require.Nil(t, command)
	require.Equal(t, writes, ws.writes.Load())
	require.Equal(t, "second", ws.Config().Models[config.SelectedModelTypeLarge].Model)
}
