package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/skills"
	"github.com/stretchr/testify/require"
)

func sessionRuntimeTestCoordinator(t *testing.T) (*coordinator, fakeEnv) {
	t.Helper()
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	env := testEnv(t)
	data := `{"options":{"disable_default_providers":true},"providers":{"first":{"type":"openai-compat","base_url":"https://first.example.invalid/v1","api_key":"fixture-first","models":[{"id":"first-model","name":"First"}]},"second":{"type":"openai-compat","base_url":"https://second.example.invalid/v1","api_key":"fixture-second","models":[{"id":"second-model","name":"Second"}]}},"models":{"large":{"provider":"first","model":"first-model"},"small":{"provider":"first","model":"first-model"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crux.json"), []byte(data), 0o600))
	store := initTestConfig(t, env.workingDir)
	return &coordinator{
		cfg:          store,
		sessions:     env.sessions,
		messages:     env.messages,
		permissions:  env.permissions,
		history:      env.history,
		filetracker:  *env.filetracker,
		currentAgent: &mockSessionAgent{},
		skillTracker: skills.NewTracker(nil),
	}, env
}

func TestSessionRuntimeUsesSavedModelsInsteadOfAnotherSessionSelection(t *testing.T) {
	coord, env := sessionRuntimeTestCoordinator(t)
	saved := coord.cfg.RuntimeSnapshot().AgentModelState()
	encoded, err := json.Marshal(saved)
	require.NoError(t, err)
	var goalIDs []string
	for _, title := range []string{"translation", "categorization"} {
		current, err := env.sessions.Create(t.Context(), title)
		require.NoError(t, err)
		_, err = env.sessions.SetModels(t.Context(), current.ID, current.ModelRevision, encoded)
		require.NoError(t, err)
		goalIDs = append(goalIDs, current.ID)
	}
	owner, ok := coord.cfg.RuntimeSnapshot().ProviderOwner("second")
	require.True(t, ok)
	selected := &config.OwnedSelectedModel{Owner: owner, Model: config.SelectedModel{Provider: "second", Model: "second-model"}}
	_, err = coord.cfg.OverrideModelsForOwners(config.AgentModelState{Large: selected, Small: selected})
	require.NoError(t, err)
	type builtRuntime struct {
		runtime InstalledRuntime
		err     error
	}
	results := make(chan builtRuntime, len(goalIDs))
	for _, id := range goalIDs {
		go func(id string) {
			runtime, err := coord.runtimeForSession(t.Context(), id)
			results <- builtRuntime{runtime: runtime, err: err}
		}(id)
	}
	for range goalIDs {
		result := <-results
		require.NoError(t, result.err)
		runtime := result.runtime
		require.Equal(t, saved.Large.Model, runtime.LargeModel.ModelCfg)
		require.Equal(t, saved.Small.Model, runtime.SmallModel.ModelCfg)
		require.Equal(t, "first", runtime.Snapshot.Config().Models[config.SelectedModelTypeLarge].Provider)
		ctx := context.WithValue(t.Context(), sessionRuntimeKey{}, runtime.Snapshot)
		require.Equal(t, "first", coord.modelSnapshotForContext(ctx).Config().Models[config.SelectedModelTypeLarge].Provider)
	}
	require.Equal(t, "second", coord.cfg.Config().Models[config.SelectedModelTypeLarge].Provider)
	require.Equal(t, "second", coord.modelSnapshotForContext(t.Context()).Config().Models[config.SelectedModelTypeLarge].Provider)
}

func TestSessionRuntimeRejectsMissingOrChangedOwnerWithoutFallback(t *testing.T) {
	coord, env := sessionRuntimeTestCoordinator(t)
	current, err := env.sessions.Create(t.Context(), "goal")
	require.NoError(t, err)
	_, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)
	_, err = coord.runtimeForSession(t.Context(), current.ID)
	var selectionErr *sessionModelSelectionError
	require.ErrorAs(t, err, &selectionErr)
	models := coord.cfg.RuntimeSnapshot().AgentModelState()
	models.Large.Owner.AccountNamespace = "different-owner"
	encoded, err := json.Marshal(models)
	require.NoError(t, err)
	_, err = env.sessions.SetModels(t.Context(), current.ID, current.ModelRevision, encoded)
	require.NoError(t, err)
	_, err = coord.runtimeForSession(t.Context(), current.ID)
	require.ErrorAs(t, err, &selectionErr)
	require.NotEmpty(t, goalBlockingReason(nil, err))
	require.Equal(t, "first", coord.cfg.Config().Models[config.SelectedModelTypeLarge].Provider)
}

func TestSessionRuntimeLoadsRetiredPathMaskingWithoutChangingSelection(t *testing.T) {
	for _, legacy := range []string{`null`, `{"enabled":true,"targets":["/retired/private-library"],"recursive":true}`} {
		t.Run(legacy, func(t *testing.T) {
			coord, env := sessionRuntimeTestCoordinator(t)
			models := coord.cfg.RuntimeSnapshot().AgentModelState()
			models.Large.Model.ProviderOptions = map[string]any{"precise": json.Number("9007199254740993"), "path_masking": "provider-owned-value"}
			encoded, err := json.Marshal(models)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &fields))
			fields["path_masking"] = json.RawMessage(legacy)
			encoded, err = json.Marshal(fields)
			require.NoError(t, err)
			current, err := env.sessions.Create(t.Context(), "legacy session")
			require.NoError(t, err)
			current, err = env.sessions.SetModels(t.Context(), current.ID, current.ModelRevision, encoded)
			require.NoError(t, err)
			runtime, err := coord.runtimeForSession(t.Context(), current.ID)
			require.NoError(t, err)
			require.Equal(t, current.ID, runtime.SessionID)
			require.Equal(t, models.Large.Model, runtime.LargeModel.ModelCfg)
			require.Equal(t, models.Small.Model, runtime.SmallModel.ModelCfg)
			require.Equal(t, models.Large.Owner, runtime.Snapshot.AgentModelState().Large.Owner)
			require.Equal(t, models.Small.Owner, runtime.Snapshot.AgentModelState().Small.Owner)
			unchanged, err := env.sessions.Get(t.Context(), current.ID)
			require.NoError(t, err)
			require.Equal(t, current.Models, unchanged.Models)
			require.Equal(t, current.ModelRevision, unchanged.ModelRevision)
		})
	}
}

func TestSessionRuntimeStillRejectsUnknownFieldsWithRetiredMasking(t *testing.T) {
	coord, env := sessionRuntimeTestCoordinator(t)
	encoded, err := json.Marshal(coord.cfg.RuntimeSnapshot().AgentModelState())
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &fields))
	fields["path_masking"] = json.RawMessage(`null`)
	fields["unexpected_setting"] = json.RawMessage(`true`)
	encoded, err = json.Marshal(fields)
	require.NoError(t, err)
	current, err := env.sessions.Create(t.Context(), "invalid session")
	require.NoError(t, err)
	_, err = env.sessions.SetModels(t.Context(), current.ID, current.ModelRevision, encoded)
	require.NoError(t, err)
	_, err = coord.runtimeForSession(t.Context(), current.ID)
	require.ErrorContains(t, err, `unknown field "unexpected_setting"`)
}
