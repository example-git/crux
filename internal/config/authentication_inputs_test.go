package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func authenticationInputsTestStore(t *testing.T) (*ConfigStore, string) {
	t.Helper()
	root := t.TempDir()
	store := NewTestStoreWithRegistrations(&Config{})
	store.baseEnvironment = env.NewFromMap(map[string]string{"AI_CLI_DIR": filepath.Join(root, "accounts")})
	store.effectiveEnvironment = cloneEnvironment(store.baseEnvironment)
	store.globalDataPath = filepath.Join(root, "config", "crux.json")
	return store, root
}

func TestAuthenticationInputsStablePrivateAndIndependent(t *testing.T) {
	store, root := authenticationInputsTestStore(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(store.globalDataPath), 0o700))
	secret := []byte(`{"private":"synthetic-input-secret"}`)
	require.NoError(t, os.WriteFile(store.globalDataPath, secret, 0o600))
	before := store.RuntimeSnapshot()
	first, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	second, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.True(t, first.SameObservation(second))
	require.True(t, before.SamePublication(store.RuntimeSnapshot()), "input reads must not publish configuration")
	file, found := first.inputs.file(store.globalDataPath)
	require.True(t, found)
	require.True(t, file.info.exists)
	require.Equal(t, secret, file.data)
	file.data[0] = '!'
	retained, _ := first.inputs.file(store.globalDataPath)
	require.Equal(t, secret, retained.data)
	_, err = json.Marshal(first.inputs)
	require.ErrorContains(t, err, "inputs are private")
	_, err = json.Marshal(retained)
	require.ErrorContains(t, err, "preimages are private")
	for _, value := range []any{first, first.inputs, retained} {
		for _, format := range []string{"%v", "%+v", "%#v", "%q", "%x"} {
			printed := fmt.Sprintf(format, value)
			require.NotContains(t, printed, "synthetic-input-secret")
			require.NotContains(t, printed, root)
		}
	}
}

func TestAuthenticationInputsBodyMetadataReplacementAndExistence(t *testing.T) {
	store, _ := authenticationInputsTestStore(t)
	capture := func() AuthenticationCapture {
		result, err := store.CaptureAuthentication(t.Context())
		require.NoError(t, err)
		return result
	}
	missing := capture()
	file, found := missing.inputs.file(store.globalDataPath)
	require.True(t, found)
	require.False(t, file.info.exists)
	require.NoError(t, os.MkdirAll(filepath.Dir(store.globalDataPath), 0o700))
	require.NoError(t, os.WriteFile(store.globalDataPath, []byte(`{"key":"first"}`), 0o600))
	created := capture()
	require.False(t, missing.SameObservation(created))
	require.True(t, missing.runtime.SamePublication(created.runtime))
	require.NoError(t, os.WriteFile(store.globalDataPath, []byte(`{"key":"other"}`), 0o600))
	changed := capture()
	require.False(t, created.SameObservation(changed))
	before, err := os.Stat(store.globalDataPath)
	require.NoError(t, err)
	require.NoError(t, os.Chtimes(store.globalDataPath, before.ModTime(), before.ModTime().Add(time.Second)))
	metadata := capture()
	require.False(t, changed.SameObservation(metadata))
	old, _ := metadata.inputs.file(store.globalDataPath)
	temporary := store.globalDataPath + ".replacement"
	require.NoError(t, os.WriteFile(temporary, old.data, 0o600))
	require.NoError(t, os.Chtimes(temporary, before.ModTime(), time.Unix(0, old.info.modified)))
	require.NoError(t, os.Rename(temporary, store.globalDataPath))
	replaced := capture()
	require.False(t, metadata.SameObservation(replaced), "identical bytes and mtime must not conceal replacement")
	require.NoError(t, os.Remove(store.globalDataPath))
	deleted := capture()
	require.False(t, replaced.SameObservation(deleted))
	_, found = missing.inputs.file("not-captured")
	require.False(t, found)
}

func TestAuthenticationInputsLoadedStoreDiscoversShellWithoutExecution(t *testing.T) {
	store, root := runtimeControlTestStore(t, "generic-json", []manifest.RuntimeControl{})
	before := store.RuntimeSnapshot()
	first, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	global := filepath.Join(root, "config", "crux.json")
	project := filepath.Join(store.workingDir, "crux.json")
	shell := filepath.Join(store.workingDir, ".cruxrc")
	marker := filepath.Join(root, "must-not-execute")
	require.NoError(t, os.WriteFile(project, []byte(`{"providers":{}}`), 0o600))
	// Deliberately not JSON. Authentication status reads the source verbatim;
	// execution here would create a marker and fail JSON parsing afterward.
	script := []byte("touch '" + marker + "'\nthis is raw configuration input\n")
	require.NoError(t, os.WriteFile(shell, script, 0o700))
	second, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.False(t, first.SameObservation(second))
	require.True(t, before.SamePublication(second.runtime))
	require.Less(t, slices.Index(second.inputs.order, global), slices.Index(second.inputs.order, project))
	require.Less(t, slices.Index(second.inputs.order, project), slices.Index(second.inputs.order, shell))
	file, found := second.inputs.file(shell)
	require.True(t, found)
	require.Equal(t, script, file.data)
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.Remove(shell))
	third, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.False(t, second.SameObservation(third))
}

func TestAuthenticationInputsPathlessAndCapturedEnvironment(t *testing.T) {
	store, root := authenticationInputsTestStore(t)
	store.globalDataPath = ""
	store.baseEnvironment = nil
	store.effectiveEnvironment = env.NewFromMap(map[string]string{"AI_CLI_DIR": filepath.Join(root, "accounts")})
	live := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA"} {
		t.Setenv(key, live)
	}
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.True(t, capture.inputs.valid)
	require.Empty(t, capture.inputs.order)
	require.Empty(t, capture.inputs.files)
	entries, err := os.ReadDir(live)
	require.NoError(t, err)
	require.Empty(t, entries)

	homeVariable, opposite := "HOME", "USERPROFILE"
	if runtime.GOOS == "windows" {
		homeVariable, opposite = opposite, homeVariable
	}
	captured := t.TempDir()
	paths, err := authenticationGlobalInputPaths(env.NewFromMap(map[string]string{homeVariable: captured, opposite: live}))
	require.NoError(t, err)
	for _, path := range paths {
		require.NotContains(t, path, live)
	}
	_, err = authenticationGlobalInputPaths(env.NewFromMap(map[string]string{opposite: live}))
	require.Error(t, err)
	_, err = authenticationGlobalInputPaths(nil)
	require.Error(t, err)
	_, err = authenticationGlobalInputPaths(env.NewFromMap(map[string]string{"CRUX_GLOBAL_CONFIG": captured, "CRUX_GLOBAL_DATA": captured}))
	require.NoError(t, err, "explicit captured paths need no home fallback")
	store.workingDir = captured
	_, err = store.CaptureAuthentication(t.Context())
	require.ErrorContains(t, err, "configuration inputs cannot be read")
}

func TestAuthenticationInputsReadOnlySourcesAndRedactedErrors(t *testing.T) {
	store, _ := authenticationInputsTestStore(t)
	directory := filepath.Dir(store.globalDataPath)
	require.NoError(t, os.MkdirAll(directory, 0o700))
	require.NoError(t, os.WriteFile(store.globalDataPath, []byte("raw secret $(not-executed)"), 0o400))
	require.NoError(t, os.Chmod(directory, 0o500))
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })
	_, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 1, "a read-only config directory must not acquire lock files")
	require.NoError(t, os.Chmod(directory, 0o700))
	secretPath := filepath.Join(directory, "private-path-and-secret")
	require.NoError(t, os.Mkdir(secretPath, 0o700))
	store.globalDataPath = secretPath
	_, err = store.CaptureAuthentication(t.Context())
	require.ErrorContains(t, err, "configuration inputs cannot be read")
	require.NotContains(t, err.Error(), "private-path-and-secret")
	require.NotContains(t, err.Error(), "not-executed")
}

func TestAuthenticationInputsCancellationAndObservedInterleaving(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := readAuthenticationInput(ctx, "never-opened")
	require.ErrorIs(t, err, context.Canceled)
	reader := authenticationInputReader{ctx: ctx, reader: bytes.NewReader([]byte("never read"))}
	n, err := reader.Read(make([]byte, 10))
	require.Zero(t, n)
	require.ErrorIs(t, err, context.Canceled)
	store, owner, entry, _ := authenticationCaptureTestStore(t)
	first, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.NoError(t, accounts.Save(t.Context(), owner.AccountNamespace, entry))
	second, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.ErrorContains(t, validateAuthenticationObservations(first.accounts, second.accounts, first.inputs, second.inputs), "account store changed during capture")
	require.NoError(t, validateAuthenticationObservations(second.accounts, second.accounts, second.inputs, second.inputs))
	changed := second.inputs
	changed.order = []string{"different topology"}
	require.ErrorIs(t, validateAuthenticationObservations(second.accounts, second.accounts, second.inputs, changed), errAuthenticationInputsChanged)
}
