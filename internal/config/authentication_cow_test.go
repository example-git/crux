package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationBasisTypedCOWTopology(t *testing.T) {
	for _, topology := range []string{"parent alias", "created project occurrence"} {
		t.Run(topology, func(t *testing.T) {
			store, _, base := authenticationBasisStore(t, func(root string, values map[string]string) {
				if topology == "parent alias" {
					require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
					require.NoError(t, os.WriteFile(filepath.Join(root, "data", "crux.json"), []byte(`{}`), 0o600))
					if err := os.Symlink(filepath.Join(root, "data"), filepath.Join(root, "workspace")); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
					return
				}
				require.NoError(t, os.MkdirAll(filepath.Join(root, "config"), 0o700))
				path := filepath.Join(root, "config", "crux.json")
				require.NoError(t, os.Rename(filepath.Join(root, "crux.json"), path))
				require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))
				values["CRUX_GLOBAL_DATA"] = root
			})
			capture, layers := authenticationBasisCapture(t, store, base)
			require.NoError(t, capture.validateConfigBasis(layers, ""), "loaded basis starts accepted")
			previous := store.Config()
			previousBasis := previous.authenticationBasis.clone()
			if topology == "created project occurrence" {
				require.NoFileExists(t, store.globalDataPath)
			}
			require.NoError(t, store.SetCompactMode(ScopeGlobal, true))
			require.True(t, store.Config().Options.TUI.CompactMode)
			require.NotSame(t, previous, store.Config())
			require.True(t, reflect.DeepEqual(previousBasis, previous.authenticationBasis))
			capture, layers = authenticationBasisCapture(t, store, base)
			require.NoError(t, capture.validateConfigBasis(layers, ""), "the typed write must preserve its own accepted basis")
		})
	}
}

func TestAuthenticationBasisTypedCOWRejectsLatePeerWrite(t *testing.T) {
	store, _, _ := authenticationBasisStore(t, nil)
	previous := store.Config()
	basisBefore := previous.authenticationBasis.clone()
	var peer []byte
	store.writeFields = func(scope Scope, fields map[string]any) error {
		if err := store.writeConfigFields(scope, fields); err != nil {
			return err
		}
		var err error
		peer, err = os.ReadFile(store.globalDataPath)
		if err != nil {
			return err
		}
		peer, err = runtimeControlChangeField(peer, []string{"peer"}, []byte(`"preserved"`), false)
		if err != nil {
			return err
		}
		return os.WriteFile(store.globalDataPath, peer, 0o600)
	}
	err := store.SetCompactMode(ScopeGlobal, true)
	require.ErrorContains(t, err, "config file updated but failed to publish")
	require.Same(t, previous, store.Config())
	require.True(t, reflect.DeepEqual(basisBefore, previous.authenticationBasis))
	actual, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Equal(t, peer, actual, "a late peer edit must not become accepted or be rolled back")
}

func authenticationCOWAlias(t *testing.T, store *ConfigStore, root string) *ConfigStore {
	t.Helper()
	alias := filepath.Join(root, "aliased-workspace")
	if err := os.Symlink(filepath.Dir(store.globalDataPath), alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	reloaded, err := LoadIsolated(store.workingDir, alias, false, store.baseEnvironment)
	require.NoError(t, err)
	return reloaded
}

func TestAuthenticationBasisRuntimeControlAliasPreview(t *testing.T) {
	control := manifest.RuntimeControl{ID: "vendor.mode", Label: "Mode", Type: "boolean", Scope: "model", RequestPath: "/vendor/mode", Default: true}
	store, root := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{control})
	target := runtimeControlTestTarget(t, store, control.ID)
	_, err := store.SetRuntimeControl(t.Context(), ScopeGlobal, target, json.RawMessage(`true`))
	require.NoError(t, err)
	store = authenticationCOWAlias(t, store, root)
	target = runtimeControlTestTarget(t, store, control.ID)
	state, err := store.SetRuntimeControl(t.Context(), ScopeGlobal, target, json.RawMessage(`false`))
	require.NoError(t, err)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, json.RawMessage(`false`)))
	capture, layers := authenticationBasisCapture(t, store, store.baseEnvironment)
	require.NoError(t, capture.validateConfigBasis(layers, ""))
	state, err = store.RemoveRuntimeControl(t.Context(), ScopeGlobal, target)
	require.NoError(t, err)
	require.False(t, state.Scoped.Present)
	require.True(t, RuntimeControlJSONEqual(state.Effective.Value, json.RawMessage(`true`)))
}

func TestAuthenticationBasisToolingAliasPreview(t *testing.T) {
	store, root := providerToolingTestStore(t)
	owner, _ := store.Config().ProviderOwner("codex")
	require.NoError(t, store.SetProviderToolingInstructions(ScopeGlobal, owner, ToolingInstructionsCrux))
	store = authenticationCOWAlias(t, store, root)
	require.NoError(t, store.SetProviderToolingInstructions(ScopeGlobal, owner, ToolingInstructionsNative))
	provider, _ := store.Config().Providers.Get("codex")
	require.Equal(t, ToolingInstructionsNative, provider.ToolingInstructions)
	capture, layers := authenticationBasisCapture(t, store, store.baseEnvironment)
	require.NoError(t, capture.validateConfigBasis(layers, ""))
	require.NoError(t, store.RemoveProviderToolingInstructions(ScopeGlobal, owner))
	provider, _ = store.Config().Providers.Get("codex")
	require.Empty(t, provider.ToolingInstructions)
}
