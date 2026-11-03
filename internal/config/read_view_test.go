package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func TestConfigReadViewAcceptedScopes(t *testing.T) {
	store, root, _ := authenticationBasisStore(t, func(root string, _ map[string]string) {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "config"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "config", "crux.json"), []byte(`{"models":{"large":{"provider":"fixture","model":"small"}},"memories":{"consolidation_model":"global-memory"}}`), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "workspace"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "workspace", "crux.json"), []byte(`{"memories":{"consolidation_model":"workspace-memory"}}`), 0o600))
	})
	global, err := store.ReadConfigView(t.Context(), ScopeGlobal)
	require.NoError(t, err)
	require.NoError(t, global.Validate())
	require.Equal(t, "small", global.Values["models.large.model"])
	require.Equal(t, "global-memory", global.Values["memories.consolidation_model"])
	require.Equal(t, "user", global.Origins["models.large.model"].Kind)
	require.Equal(t, filepath.Join(root, "config", "crux.json"), global.Origins["models.large.model"].Path)
	workspace, err := store.ReadConfigView(t.Context(), ScopeWorkspace)
	require.NoError(t, err)
	require.NoError(t, workspace.Validate())
	require.Equal(t, "main", workspace.Values["models.large.model"])
	require.Equal(t, "project", workspace.Origins["models.large.model"].Kind)
	require.Equal(t, "workspace-memory", workspace.Values["memories.consolidation_model"])
	require.Equal(t, "workspace", workspace.Origins["memories.consolidation_model"].Kind)
	authenticationBasisWriteField(t, filepath.Join(root, "config", "crux.json"), []string{"models", "large", "model"}, `"edited-on-disk"`)
	store.baseEnvironment = env.NewFromMap(map[string]string{"CRUX_GLOBAL_CONFIG": filepath.Join(root, "elsewhere")})
	again, err := store.ReadConfigView(t.Context(), ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, global, again)
	store.overrides.Models = map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: "fixture", Model: "pinned"}}
	for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace} {
		pinned, err := store.ReadConfigView(t.Context(), scope)
		require.NoError(t, err)
		require.Equal(t, "pinned", pinned.Values["models.large.model"])
		require.Equal(t, "runtime", pinned.Origins["models.large.model"].Kind)
		require.Empty(t, pinned.Origins["models.large.model"].Path)
		require.NoError(t, pinned.Validate())
	}
}

func TestCollectedConfigReadViewRetainsAcceptedProvenance(t *testing.T) {
	store, root, _ := authenticationBasisStore(t, nil)
	for _, pinned := range []bool{false, true} {
		store.overrides.Models = nil
		if pinned {
			store.overrides.Models = map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: "fixture", Model: "main"}}
		}
		expected, err := store.ReadConfigView(t.Context(), ScopeWorkspace)
		require.NoError(t, err)
		proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
		require.NoError(t, err)
		store.overrides.Models = map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: "fixture", Model: "small"}}
		authenticationBasisWriteField(t, filepath.Join(root, "crux.json"), []string{"models", "large", "model"}, `"small"`)
		view, err := proposal.ReadConfigView(t.Context(), ScopeWorkspace)
		require.NoError(t, err)
		require.Equal(t, expected, view)
		require.NoError(t, view.Validate())
		view.Values["models.large.model"] = "mutated"
		again, err := proposal.ReadConfigView(t.Context(), ScopeWorkspace)
		require.NoError(t, err)
		require.Equal(t, expected, again)
		data, err := json.Marshal(proposal)
		require.NoError(t, err)
		var decoded RemoteRuntimeProposal
		require.NoError(t, json.Unmarshal(data, &decoded))
		_, err = decoded.ReadConfigView(t.Context(), ScopeWorkspace)
		require.ErrorContains(t, err, "provenance is unavailable")
		require.NoError(t, decoded.RetainCollectionSource(proposal))
		restored, err := decoded.ReadConfigView(t.Context(), ScopeWorkspace)
		require.NoError(t, err)
		require.Equal(t, expected, restored)
		_, err = proposal.ReadConfigView(t.Context(), Scope(-1))
		require.ErrorContains(t, err, "scope")
	}
}

func TestConfigReadViewInheritedOrigins(t *testing.T) {
	const lower = `{"models":{"large":{"provider":"fixture","model":"small"}},"memories":{"consolidation_model":"global-memory"}}`
	for _, test := range []struct {
		name  string
		upper string
	}{
		{name: "null models", upper: `{"models":null}`},
		{name: "null large model", upper: `{"models":{"large":null}}`},
		{name: "null memories", upper: `{"memories":null}`},
		{name: "same explicit values", upper: lower},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, root, _ := authenticationBasisStore(t, func(root string, _ map[string]string) {
				for directory, data := range map[string]string{"config": lower, "data": test.upper} {
					require.NoError(t, os.MkdirAll(filepath.Join(root, directory), 0o700))
					require.NoError(t, os.WriteFile(filepath.Join(root, directory, "crux.json"), []byte(data), 0o600))
				}
			})
			view, err := store.ReadConfigView(t.Context(), ScopeGlobal)
			require.NoError(t, err)
			require.NoError(t, view.Validate())
			expected := map[string]string{"models.large.model": "small", "models.large.provider": "fixture", "memories.consolidation_model": "global-memory"}
			require.Equal(t, expected, view.Values)
			directory := "config"
			if test.upper == lower {
				directory = "data"
			}
			for field := range expected {
				require.Equal(t, ConfigReadOrigin{Kind: "user", Path: filepath.Join(root, directory, "crux.json"), Version: configReadVersion(expected)}, view.Origins[field], field)
			}
		})
	}
}

func TestConfigReadViewDoesNotReevaluateShell(t *testing.T) {
	store, root, _ := authenticationBasisStore(t, func(root string, _ map[string]string) {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "config"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "config", "cruxrc"), []byte("model large fixture/small\n"), 0o600))
	})
	before, err := store.ReadConfigView(t.Context(), ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, "small", before.Values["models.large.model"])
	require.Equal(t, filepath.Join(root, "config", "cruxrc"), before.Origins["models.large.model"].Path)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config", "cruxrc"), []byte("exit 99\n"), 0o600))
	after, err := store.ReadConfigView(t.Context(), ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestConfigReadViewRejectsUnavailableAuthority(t *testing.T) {
	store := NewTestStore(&Config{})
	_, err := store.ReadConfigView(t.Context(), ScopeGlobal)
	require.ErrorContains(t, err, "provenance is unavailable")
	store.clientRuntime = &clientRuntimeState{}
	_, err = store.ReadConfigView(t.Context(), ScopeGlobal)
	require.ErrorIs(t, err, ErrClientRuntimeManaged)
	_, err = store.ReadConfigView(t.Context(), Scope(-1))
	require.ErrorContains(t, err, "scope")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.ReadConfigView(ctx, ScopeGlobal)
	require.ErrorIs(t, err, context.Canceled)
}
