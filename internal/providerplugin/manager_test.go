package providerplugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestManagerInstallTrustAndDigestChange(t *testing.T) {
	manager := newTestManager(t)
	initial := manager.Snapshot()
	require.Equal(t, uint64(1), initial.Revision)
	require.Empty(t, initial.Plugins)

	snapshot, err := manager.Install(t.Context(), InstallRequest{
		Source:           exampleBundle(t, "minimal.plugin"),
		ExpectedRevision: initial.Revision,
	})
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 1)
	status := snapshot.Plugins[0]
	require.Equal(t, StateUntrusted, status.State)
	require.Equal(t, TrustUnknown, status.Trust)
	require.Len(t, status.Digest, 64)

	snapshot, err = manager.SetTrust(t.Context(), status.ID, TrustRequest{
		Digest:           status.Digest,
		Trusted:          true,
		ExpectedRevision: snapshot.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, StateRegistered, snapshot.Plugins[0].State)
	require.Equal(t, TrustTrusted, snapshot.Plugins[0].Trust)

	manifestPath := filepath.Join(manager.paths.Bundles, status.BundleName, manifestFilename)
	data, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(data, &value))
	value["description"] = "Changed after exact-digest approval"
	data, err = json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o600))

	snapshot, err = manager.Rescan(t.Context(), snapshot.Revision)
	require.NoError(t, err)
	require.Equal(t, StateUntrusted, snapshot.Plugins[0].State)
	require.Equal(t, TrustUnknown, snapshot.Plugins[0].Trust)
	require.NotEqual(t, status.Digest, snapshot.Plugins[0].Digest)
}

func TestManagerExplicitInstallTrustsOnlyCommittedDigest(t *testing.T) {
	manager := newTestManager(t)
	source := filepath.Join(t.TempDir(), "minimal.plugin")
	value := readExampleManifest(t)
	writeBundleManifest(t, source, value)

	snapshot, err := manager.Install(t.Context(), InstallRequest{
		Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
	require.Equal(t, StateRegistered, snapshot.Plugins[0].State)
	require.Equal(t, TrustTrusted, snapshot.Plugins[0].Trust)
	initialDigest := snapshot.Plugins[0].Digest

	value.Description = "Explicitly updated bundle"
	writeBundleManifest(t, source, value)
	snapshot, err = manager.Install(t.Context(), InstallRequest{
		Source: source, Update: true, Trust: true, ExpectedRevision: snapshot.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, StateRegistered, snapshot.Plugins[0].State)
	require.Equal(t, TrustTrusted, snapshot.Plugins[0].Trust)
	require.NotEqual(t, initialDigest, snapshot.Plugins[0].Digest)

	manifestPath := filepath.Join(manager.paths.Bundles, snapshot.Plugins[0].BundleName, manifestFilename)
	value.Description = "Changed outside explicit installation"
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o600))
	snapshot, err = manager.Rescan(t.Context(), snapshot.Revision)
	require.NoError(t, err)
	require.Equal(t, StateUntrusted, snapshot.Plugins[0].State)
	require.Equal(t, TrustUnknown, snapshot.Plugins[0].Trust)
}

func TestManagerRetainsValidatedStaticTextWithoutExposingMutableState(t *testing.T) {
	manager := newTestManager(t)
	snapshot, err := manager.Install(t.Context(), InstallRequest{
		Source: exampleBundle(t, "responses-oauth.plugin"), ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 1)
	snapshot, err = manager.SetTrust(t.Context(), snapshot.Plugins[0].ID, TrustRequest{
		Digest: snapshot.Plugins[0].Digest, Trusted: true, ExpectedRevision: snapshot.Revision,
	})
	require.NoError(t, err)

	bundles := manager.RegisteredBundles()
	require.Len(t, bundles, 1)
	path := bundles[0].Manifest.Capabilities.Instructions.Profiles["native"]
	require.NotEmpty(t, bundles[0].StaticText[path])
	bundles[0].StaticText[path] = "mutated"

	bundles = manager.RegisteredBundles()
	require.NotEqual(t, "mutated", bundles[0].StaticText[path])
}

func TestManagerRejectsUnsafeSourceAndPreservesInstalledUpdate(t *testing.T) {
	manager := newTestManager(t)
	snapshot, err := manager.Install(t.Context(), InstallRequest{Source: exampleBundle(t, "minimal.plugin"), ExpectedRevision: 1})
	require.NoError(t, err)
	original := snapshot.Plugins[0]

	unsafe := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(exampleBundle(t, "minimal.plugin"), manifestFilename), filepath.Join(unsafe, manifestFilename)))
	_, err = manager.Install(t.Context(), InstallRequest{Source: unsafe, Update: true, ExpectedRevision: snapshot.Revision})
	require.Error(t, err)

	current := manager.Snapshot()
	require.Equal(t, original.Digest, current.Plugins[0].Digest)
	require.Equal(t, original.BundleName, current.Plugins[0].BundleName)
}

func TestManagerQuarantinesDuplicateProviderClaims(t *testing.T) {
	manager := newTestManager(t)
	first := readExampleManifest(t)
	second := first
	second.ID = "second-example"
	second.Name = "Second Example"

	writeBundleManifest(t, filepath.Join(manager.paths.Bundles, first.ID+bundleSuffix), first)
	writeBundleManifest(t, filepath.Join(manager.paths.Bundles, second.ID+bundleSuffix), second)
	snapshot, err := manager.Rescan(t.Context(), manager.Snapshot().Revision)
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 2)
	for _, status := range snapshot.Plugins {
		require.Equal(t, StateQuarantined, status.State)
		require.Contains(t, diagnosticCodes(status), "duplicate-provider-id")
	}
}

func TestManagerQuarantinesDuplicatePluginIDs(t *testing.T) {
	manager := newTestManager(t)
	first := readExampleManifest(t)
	second := first
	second.Provider.ID = "second-provider"
	second.Provider.Name = "Second Provider"

	writeBundleManifest(t, filepath.Join(manager.paths.Bundles, "first"+bundleSuffix), first)
	writeBundleManifest(t, filepath.Join(manager.paths.Bundles, "second"+bundleSuffix), second)
	snapshot, err := manager.Rescan(t.Context(), manager.Snapshot().Revision)
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 2)
	for _, status := range snapshot.Plugins {
		require.Equal(t, StateQuarantined, status.State)
		require.Contains(t, diagnosticCodes(status), "duplicate-plugin-id")
	}
}

func TestManagerTrustRevocationPersistsAndCanBeReapproved(t *testing.T) {
	root := t.TempDir()
	paths := testPaths(root)
	manager, err := NewManager(t.Context(), paths)
	require.NoError(t, err)
	snapshot, err := manager.Install(t.Context(), InstallRequest{Source: exampleBundle(t, "minimal.plugin")})
	require.NoError(t, err)
	status := snapshot.Plugins[0]

	snapshot, err = manager.SetTrust(t.Context(), status.ID, TrustRequest{
		Digest: status.Digest, Trusted: true, ExpectedRevision: snapshot.Revision,
	})
	require.NoError(t, err)
	snapshot, err = manager.SetTrust(t.Context(), status.ID, TrustRequest{
		Digest: status.Digest, Trusted: false, ExpectedRevision: snapshot.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, StateQuarantined, snapshot.Plugins[0].State)
	require.Equal(t, TrustRevoked, snapshot.Plugins[0].Trust)
	require.Contains(t, diagnosticCodes(snapshot.Plugins[0]), "trust-revoked")
	manager.Close()

	manager, err = NewManager(t.Context(), paths)
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	snapshot = manager.Snapshot()
	require.Equal(t, StateQuarantined, snapshot.Plugins[0].State)
	require.Equal(t, TrustRevoked, snapshot.Plugins[0].Trust)

	_, err = manager.SetTrust(t.Context(), status.ID, TrustRequest{
		Digest: status.Digest, Trusted: true, ExpectedRevision: snapshot.Revision + 1,
	})
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = manager.SetTrust(t.Context(), status.ID, TrustRequest{Digest: string(make([]byte, 64)), Trusted: true})
	require.ErrorIs(t, err, ErrPluginMissing)

	snapshot, err = manager.SetTrust(t.Context(), status.ID, TrustRequest{
		Digest: status.Digest, Trusted: true, ExpectedRevision: snapshot.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, StateRegistered, snapshot.Plugins[0].State)
	require.Equal(t, TrustTrusted, snapshot.Plugins[0].Trust)
}

func TestManagerReportsIncompatibleInstalledBundle(t *testing.T) {
	manager := newTestManager(t)
	value := readExampleManifest(t)
	value.Compatibility.HostAPI = manifest.VersionBounds{Min: manifest.HostAPIVersion + 1, Max: manifest.HostAPIVersion + 1}
	writeBundleManifest(t, filepath.Join(manager.paths.Bundles, value.ID+bundleSuffix), value)

	snapshot, err := manager.Rescan(t.Context(), manager.Snapshot().Revision)
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 1)
	require.Equal(t, StateIncompatible, snapshot.Plugins[0].State)
	require.Equal(t, CompatibilityIncompatible, snapshot.Plugins[0].Compatibility)
	require.Empty(t, manager.RegisteredBundles())
	_, err = manager.SetTrust(t.Context(), value.ID, TrustRequest{Digest: snapshot.Plugins[0].Digest, Trusted: true})
	require.ErrorContains(t, err, "not eligible")
}

func TestTrustStoreRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	paths := testPaths(root)
	require.NoError(t, os.MkdirAll(paths.State, 0o700))
	target := filepath.Join(root, "target.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"version":1,"records":{}}`), 0o600))
	require.NoError(t, os.Symlink(target, paths.TrustFile))
	_, err := NewManager(context.Background(), paths)
	require.Error(t, err)
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := NewManager(t.Context(), testPaths(t.TempDir()))
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	return manager
}

func testPaths(root string) Paths {
	state := filepath.Join(root, "state")
	return Paths{
		Root:           root,
		Bundles:        filepath.Join(root, "plugins"),
		Cache:          filepath.Join(root, "cache"),
		State:          state,
		TrustFile:      filepath.Join(state, trustFilename),
		ProvenanceFile: filepath.Join(state, provenanceFilename),
		ManagerLock:    filepath.Join(state, managerLockName),
	}
}

func exampleBundle(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", name))
	require.NoError(t, err)
	return path
}

func readExampleManifest(t *testing.T) manifest.Manifest {
	t.Helper()
	return readExampleManifestNamed(t, "minimal.plugin")
}

func readExampleManifestNamed(t *testing.T, name string) manifest.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(exampleBundle(t, name), manifestFilename))
	require.NoError(t, err)
	value, err := manifest.DecodeStrict(data)
	require.NoError(t, err)
	return value
}

func writeBundleManifest(t *testing.T, root string, value manifest.Manifest) {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o700))
	data, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, manifestFilename), data, 0o600))
}

func diagnosticCodes(status Status) []string {
	result := make([]string, 0, len(status.Diagnostics))
	for _, diagnostic := range status.Diagnostics {
		result = append(result, diagnostic.Code)
	}
	return result
}
