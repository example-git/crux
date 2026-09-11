package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationBasisProjectionOmitsAbsentPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	base := env.NewFromMap(map[string]string{
		"HOME": root, "USERPROFILE": root,
		"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"),
		"CRUX_GLOBAL_DATA":   filepath.Join(root, "data"),
	})
	paths := lookupConfigsFromEnvironment(root, base, true)
	require.NotContains(t, paths, "")
	globalPaths, err := authenticationGlobalInputPaths(base)
	require.NoError(t, err)
	require.Equal(t, paths, globalPaths)
	basis := newAuthenticationLoadBasis()
	for _, path := range paths {
		basis.source(path, nil, nil, false)
	}
	target := filepath.Join(root, "data", "crux.json")
	next, written, err := projectAuthenticationBasisWrites(t.Context(), basis, []authenticationConfigWrite{{path: target, fields: map[string]any{"options.notifications": "disabled"}}}, root, "", base, true)
	require.NoError(t, err)
	require.Equal(t, basis.order, next.order)
	require.NotContains(t, next.order, ".")
	require.Contains(t, written, target)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
	require.NoError(t, os.WriteFile(target, next.sources[target].raw, 0o600))
	require.NoError(t, verifyAuthenticationWriteTopology(t.Context(), next, written, root, "", base, true))
	require.ErrorIs(t, verifyAuthenticationWriteTopology(t.Context(), next, written, root, filepath.Join(root, "other.json"), base, true), errAuthenticationInputsChanged)
}

func TestAuthenticationBasisStartupAndReloadAuthoredTopology(t *testing.T) {
	for _, mode := range []string{"startup", "reload"} {
		for _, topology := range []string{"parent alias", "created project occurrence", "authored leaf chain"} {
			t.Run(mode+"/"+topology, func(t *testing.T) {
				root := t.TempDir()
				values := map[string]string{
					"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"),
					"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"),
					"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(ProviderProfileCoreOnly),
				}
				for _, name := range []string{"config", "data", "workspace"} {
					require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o700))
				}
				source := []byte(`{"options":{"disable_default_providers":true},"providers":{"fixture":{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"synthetic","models":[{"id":"main"}]}},"models":{"large":{"provider":"fixture","model":"main"},"small":{"provider":"fixture","model":"main"}}}`)
				require.NoError(t, os.WriteFile(filepath.Join(root, "config", "crux.json"), source, 0o600))
				workspace := filepath.Join(root, "workspace")
				switch topology {
				case "parent alias":
					workspace = filepath.Join(root, "linked-data")
					if err := os.Symlink(filepath.Join(root, "data"), workspace); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				case "created project occurrence":
					values["CRUX_GLOBAL_DATA"] = root
				}
				global := filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json")
				reset := func() {
					switch topology {
					case "authored leaf chain":
						data, err := runtimeControlChangeField(source, []string{"options", "disable_notifications"}, []byte(`true`), false)
						require.NoError(t, err)
						require.NoError(t, os.WriteFile(global, data, 0o600))
						configPath := filepath.Join(root, "config", "crux.json")
						require.NoError(t, os.Remove(configPath))
						if err := os.Symlink(global, configPath); err != nil {
							t.Skipf("symlink unavailable: %v", err)
						}
						workspacePath := filepath.Join(workspace, "crux.json")
						_ = os.Remove(workspacePath)
						require.NoError(t, os.Symlink(configPath, workspacePath))
					case "parent alias":
						require.NoError(t, os.WriteFile(global, []byte(`{"foreign":{"precise":9007199254740993}}`), 0o600))
					default:
						err := os.Remove(global)
						require.True(t, err == nil || os.IsNotExist(err))
					}
				}
				reset()
				base := env.NewFromMap(values)
				store, err := LoadIsolated(root, workspace, false, base)
				require.NoError(t, err)
				if mode == "reload" {
					reset()
					var prepared *Config
					var basisBefore *authenticationLoadBasis
					store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
						prepared = snapshot.Config()
						basisBefore = prepared.authenticationBasis.clone()
						return RuntimeGenerationCandidate{Commit: func() {}, Abort: func() {}}, nil
					})
					require.NoError(t, store.ReloadFromDisk(t.Context()))
					require.Same(t, prepared, store.Config())
					require.True(t, reflect.DeepEqual(basisBefore, prepared.authenticationBasis), "prepared basis must remain immutable")
				}
				capture, layers := authenticationBasisCapture(t, store, base)
				require.NoError(t, capture.validateConfigBasis(layers, ""), "the loader's own migration must not make authentication unavailable")
				data, err := os.ReadFile(global)
				require.NoError(t, err)
				require.Contains(t, string(data), `"owner"`)
				switch topology {
				case "parent alias":
					require.Contains(t, string(data), "9007199254740993")
					require.True(t, RuntimeControlJSONEqual(store.Config().authenticationBasis.sources[global].raw, store.Config().authenticationBasis.sources[store.workspacePath].raw))
				case "created project occurrence":
					count := 0
					for _, path := range store.Config().authenticationBasis.order {
						if path == global {
							count++
						}
					}
					require.Equal(t, 2, count, "the created global file is also a project layer")
				default:
					configData, err := os.ReadFile(filepath.Join(root, "config", "crux.json"))
					require.NoError(t, err)
					require.NotContains(t, string(configData), `"owner"`, "replaced config leaf no longer reads later data-file owner migration")
					require.NotContains(t, string(configData), "disable_notifications")
					require.Contains(t, string(data), `"notifications":"disabled"`)
				}
			})
		}
	}
}

func TestAuthenticationBasisReloadRejectsAuthoredAliasRetarget(t *testing.T) {
	store, root, _ := authenticationBasisStore(t, func(root string, _ map[string]string) {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "data", "crux.json"), []byte(`{}`), 0o600))
		if err := os.Symlink(filepath.Join(root, "data"), filepath.Join(root, "workspace")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	})
	old := store.Config()
	before := []byte(`{"foreign":"retained"}`)
	require.NoError(t, os.WriteFile(store.globalDataPath, before, 0o600))
	peerDir := filepath.Join(root, "peer")
	peer := []byte(`{"peer":"must remain"}`)
	require.NoError(t, os.Mkdir(peerDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(peerDir, "crux.json"), peer, 0o600))
	var prepared *Config
	var basisBefore *authenticationLoadBasis
	var committed, aborted bool
	store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		prepared = snapshot.Config()
		basisBefore = prepared.authenticationBasis.clone()
		require.NoError(t, os.Remove(filepath.Join(root, "workspace")))
		require.NoError(t, os.Symlink(peerDir, filepath.Join(root, "workspace")))
		return RuntimeGenerationCandidate{Commit: func() { committed = true }, Abort: func() { aborted = true }}, nil
	})
	err := store.ReloadFromDisk(t.Context())
	require.ErrorContains(t, err, "authored configuration postimage differs")
	require.False(t, committed)
	require.True(t, aborted)
	require.Same(t, old, store.Config())
	require.True(t, reflect.DeepEqual(basisBefore, prepared.authenticationBasis))
	data, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Equal(t, before, data, "roll back only the operation's own migration")
	data, err = os.ReadFile(store.workspacePath)
	require.NoError(t, err)
	require.Equal(t, peer, data)
	link, err := os.Readlink(filepath.Join(root, "workspace"))
	require.NoError(t, err)
	require.Equal(t, peerDir, link, "never restore over the peer's retargeted alias")
}
