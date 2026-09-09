package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func authenticationTopologyCapture(t *testing.T, store *ConfigStore) authenticationConfigInputs {
	t.Helper()
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	store.configMu.Lock()
	snapshot := store.runtimeSnapshotLocked(store.config, store.resolver, store.providerRegistry, store.effectiveEnvironment)
	store.configMu.Unlock()
	inputs, err := store.captureAuthenticationInputsLocked(t.Context(), snapshot)
	require.NoError(t, err)
	return inputs
}

func authenticationTopologyPrepare(t *testing.T, store *ConfigStore, inputs authenticationConfigInputs, path string) authenticationScopeTopology {
	t.Helper()
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	store.configMu.Lock()
	snapshot := store.runtimeSnapshotLocked(store.config, store.resolver, store.providerRegistry, store.effectiveEnvironment)
	store.configMu.Unlock()
	topology, err := prepareAuthenticationScopeTopology(t.Context(), snapshot, inputs, path,
		store.workingDir, store.workspacePath, store.baseEnvironment)
	require.NoError(t, err)
	return topology
}

func authenticationTopologyReplace(t *testing.T, store *ConfigStore, topology authenticationScopeTopology, path string) {
	t.Helper()
	const authored = `{"providers":{"fixture":{"api_key":"synthetic-written-secret"}}}`
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	temporary, err := os.CreateTemp(filepath.Dir(path), ".scope-test-*")
	require.NoError(t, err)
	_, err = temporary.WriteString(authored)
	require.NoError(t, err)
	require.NoError(t, temporary.Close())
	require.NoError(t, os.Rename(temporary.Name(), path))
	postimage, err := readAuthenticationInput(t.Context(), path)
	require.NoError(t, err)
	for i, file := range topology.inputs.files {
		if slices.Contains(topology.writtenPaths, file.path) {
			topology.inputs.files[i] = postimage
			topology.inputs.files[i].path = file.path
		}
	}
	actual := authenticationTopologyCapture(t, store)
	require.Equal(t, actual.order, topology.inputs.order)
	require.True(t, topology.inputs.sameObservation(actual), "prepared topology plus authored postimage must match actual capture")
}

func TestAuthenticationScopeTopologyCreationMatchesCapture(t *testing.T) {
	for _, test := range []struct{ name, scope, location string }{
		{"workspace-at-cwd", "workspace", "child"},
		{"global-at-cwd", "global", "child"},
		{"workspace-at-boundary", "workspace", "project"},
		{"global-at-boundary", "global", "project"},
		{"workspace-outside", "workspace", "outside/nested"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, root := authenticationInputsTestStore(t)
			boundary := filepath.Join(root, "project")
			working := filepath.Join(boundary, "child")
			for _, dir := range []string{boundary, working} {
				require.NoError(t, os.MkdirAll(dir, 0o700))
				for _, name := range []string{".cruxrc", "cruxrc", ".crux.json"} {
					require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("raw source must not execute"), 0o600))
				}
			}
			location := filepath.Join(root, test.location)
			if test.location == "child" {
				location = working
			}
			path := filepath.Join(location, "crux.json")
			store.workingDir = working
			store.workspacePath = filepath.Join(root, "workspace", "crux.json")
			store.globalDataPath = filepath.Join(root, "data", "crux.json")
			if test.scope == "workspace" {
				store.workspacePath = path
			} else {
				store.globalDataPath = path
			}
			store.baseEnvironment = env.NewFromMap(map[string]string{
				"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Dir(store.globalDataPath),
			})
			worktreeRootCache.Store(working, boundary)
			t.Cleanup(func() { worktreeRootCache.Delete(working) })
			// Retained loaded paths remain behind active discovery in files.
			store.loadedPaths = []string{filepath.Join(root, "old-source.json")}
			inputs := authenticationTopologyCapture(t, store)
			topology := authenticationTopologyPrepare(t, store, inputs, path)
			require.NoFileExists(t, path)
			require.Equal(t, []string{path}, topology.writtenPaths)
			if test.location != "outside/nested" {
				require.Equal(t, len(inputs.order)+1, len(topology.inputs.order))
				// Reverse lookup order: crux.json precedes the existing siblings.
				discovered := slices.Index(topology.inputs.order[4:], path) + 4
				require.Equal(t, filepath.Join(location, ".crux.json"), topology.inputs.order[discovered+1])
			} else {
				require.Equal(t, inputs.order, topology.inputs.order)
			}
			original := slices.Clone(inputs.order)
			topology.inputs.order[0] = "copy-isolated"
			require.Equal(t, original, inputs.order)
			topology = authenticationTopologyPrepare(t, store, inputs, path)
			authenticationTopologyReplace(t, store, topology, path)
			before, _ := inputs.file(path)
			require.False(t, before.info.exists)
		})
	}
}

func TestAuthenticationScopeTopologyActualLoadSameWorkspaceDirectory(t *testing.T) {
	_, root, base := authenticationBasisStore(t, func(root string, _ map[string]string) {
		require.NoError(t, os.Mkdir(filepath.Join(root, "config"), 0o700))
		require.NoError(t, os.Rename(filepath.Join(root, "crux.json"), filepath.Join(root, "config", "crux.json")))
	})
	store, err := LoadIsolated(root, root, false, base)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "crux.json"), store.workspacePath)
	require.NoFileExists(t, store.workspacePath)
	inputs := authenticationTopologyCapture(t, store)
	topology := authenticationTopologyPrepare(t, store, inputs, store.workspacePath)
	require.Equal(t, len(inputs.order)+1, len(topology.inputs.order))
	require.Equal(t, []string{store.workspacePath, store.workspacePath}, topology.inputs.order[len(topology.inputs.order)-2:])
	authenticationTopologyReplace(t, store, topology, store.workspacePath)
}

func TestAuthenticationScopeTopologyAliasesAndLeafReplacement(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			store, root := authenticationInputsTestStore(t)
			realDir := filepath.Join(root, "real")
			linkDir := filepath.Join(root, "linked")
			require.NoError(t, os.Mkdir(realDir, 0o700))
			if err := os.Symlink(realDir, linkDir); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			store.workingDir = linkDir
			store.workspacePath = filepath.Join(realDir, "crux.json")
			store.baseEnvironment = env.NewFromMap(map[string]string{
				"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Dir(store.globalDataPath),
			})
			worktreeRootCache.Store(linkDir, realDir)
			t.Cleanup(func() { worktreeRootCache.Delete(linkDir) })
			alias := filepath.Join(linkDir, ".crux.json")
			require.NoError(t, os.Symlink("crux.json", alias))
			unrelated := filepath.Join(realDir, "other.json")
			require.NoError(t, os.WriteFile(unrelated, []byte(`{"unrelated":"retained-secret"}`), 0o600))
			store.loadedPaths = []string{unrelated}
			if existing {
				require.NoError(t, os.Symlink(unrelated, store.workspacePath))
			}
			inputs := authenticationTopologyCapture(t, store)
			topology := authenticationTopologyPrepare(t, store, inputs, store.workspacePath)
			require.ElementsMatch(t, []string{store.workspacePath, filepath.Join(linkDir, "crux.json"), alias}, topology.writtenPaths)
			_, err := json.Marshal(topology)
			require.Error(t, err)
			for _, format := range []string{"%v", "%+v", "%#v", "%q"} {
				text := fmt.Sprintf(format, topology)
				require.NotContains(t, text, root)
				require.NotContains(t, text, "retained-secret")
			}
			authenticationTopologyReplace(t, store, topology, store.workspacePath)
			data, err := os.ReadFile(unrelated)
			require.NoError(t, err)
			require.Equal(t, `{"unrelated":"retained-secret"}`, string(data))
		})
	}
}

func TestAuthenticationScopeTopologyRejectsDriftAndDetachedBeforeLookup(t *testing.T) {
	store, root, _ := authenticationBasisStore(t, nil)
	inputs := authenticationTopologyCapture(t, store)
	foreign := filepath.Join(root, ".crux.json")
	require.NoError(t, os.WriteFile(foreign, []byte("foreign-secret"), 0o600))
	_, err := prepareAuthenticationScopeTopology(t.Context(), store.RuntimeSnapshot(), inputs, store.workspacePath,
		store.workingDir, store.workspacePath, store.baseEnvironment)
	require.ErrorIs(t, err, errAuthenticationInputsChanged)
	require.NoFileExists(t, store.workspacePath)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = prepareAuthenticationScopeTopology(ctx, RuntimeSnapshot{}, inputs, "relative", "relative", "relative", nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = prepareAuthenticationScopeTopology(t.Context(), RuntimeSnapshot{clientRuntime: &clientRuntimeState{}}, inputs, "relative", "relative", "relative", nil)
	require.ErrorIs(t, err, ErrClientRuntimeManaged)
	_, err = prepareAuthenticationScopeTopology(t.Context(), RuntimeSnapshot{}, inputs, "relative", "relative", "relative", nil)
	require.Error(t, err)

	// Even a live alias now pointing to the target cannot adopt a different
	// captured preimage. The transaction must capture/revalidate it explicitly.
	pathless, pathlessRoot := authenticationInputsTestStore(t)
	pathless.workspacePath = filepath.Join(pathlessRoot, "crux.json")
	require.NoError(t, os.WriteFile(pathless.workspacePath, []byte("first-secret"), 0o600))
	alias := filepath.Join(pathlessRoot, "alias.json")
	require.NoError(t, os.WriteFile(alias, []byte("other-secret"), 0o600))
	pathless.loadedPaths = []string{alias}
	before := authenticationTopologyCapture(t, pathless)
	require.NoError(t, os.Remove(alias))
	if err := os.Symlink(pathless.workspacePath, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err = prepareAuthenticationScopeTopology(t.Context(), pathless.RuntimeSnapshot(), before, pathless.workspacePath, "", pathless.workspacePath, nil)
	require.ErrorIs(t, err, errAuthenticationInputsChanged)
	require.False(t, bytes.Contains([]byte(err.Error()), []byte("secret")))
}
