//go:build !windows

package providerplugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestInstallRejectsHardLinks(t *testing.T) {
	manager := newTestManager(t)
	source := t.TempDir()
	data, err := os.ReadFile(filepath.Join(exampleBundle(t, "minimal.plugin"), manifestFilename))
	require.NoError(t, err)
	manifestPath := filepath.Join(source, manifestFilename)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o600))
	require.NoError(t, os.Link(manifestPath, filepath.Join(source, "duplicate.json")))
	_, err = manager.Install(t.Context(), InstallRequest{Source: source})
	require.ErrorContains(t, err, "hard-linked")
}

func TestInstallRejectsUndeclaredFiles(t *testing.T) {
	manager := newTestManager(t)
	source := t.TempDir()
	data, err := os.ReadFile(filepath.Join(exampleBundle(t, "minimal.plugin"), manifestFilename))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, manifestFilename), data, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "undeclared.txt"), []byte("not declared"), 0o600))
	_, err = manager.Install(t.Context(), InstallRequest{Source: source})
	require.ErrorContains(t, err, "undeclared file")
}

func TestInstallRejectsExecutableDeclaration(t *testing.T) {
	manager := newTestManager(t)
	source := t.TempDir()
	data, err := os.ReadFile(filepath.Join(exampleBundle(t, "minimal.plugin"), manifestFilename))
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(data, &value))
	value["entrypoint"] = map[string]any{"kind": "executable", "path": "plugin"}
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, manifestFilename), data, 0o600))
	_, err = manager.Install(t.Context(), InstallRequest{Source: source})
	require.ErrorContains(t, err, "unknown field")
}

func TestInstallRejectsNonUTF8StaticText(t *testing.T) {
	manager := newTestManager(t)
	source := t.TempDir()
	value := readExampleManifest(t)
	value.Capabilities.Instructions = &manifest.InstructionPolicy{
		Profiles: map[string]string{"native": "instructions/native.txt"},
		Default:  "native",
	}
	writeBundleManifest(t, source, value)
	require.NoError(t, os.MkdirAll(filepath.Join(source, "instructions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "instructions", "native.txt"), []byte{0xff, 0xfe}, 0o600))
	_, err := manager.Install(t.Context(), InstallRequest{Source: source})
	require.ErrorContains(t, err, "bounded UTF-8")
}

func TestInstallRejectsIncompatibleHostAPI(t *testing.T) {
	manager := newTestManager(t)
	source := t.TempDir()
	value := readExampleManifest(t)
	value.Compatibility.HostAPI = manifest.VersionBounds{Min: manifest.HostAPIVersion + 1, Max: manifest.HostAPIVersion + 1}
	writeBundleManifest(t, source, value)
	_, err := manager.Install(t.Context(), InstallRequest{Source: source})
	require.ErrorContains(t, err, "host API")
}

func TestManifestReadBoundAppliedBeforeDecode(t *testing.T) {
	root := t.TempDir()
	data := make([]byte, manifest.MaxManifestBytes+1)
	data[0] = '{'
	require.NoError(t, os.WriteFile(filepath.Join(root, manifestFilename), data, 0o600))
	snapshot, err := snapshotDirectory(root, filepath.Join(t.TempDir(), "snapshot"))
	require.NoError(t, err)
	_, diagnostics := validateSnapshot(filepath.Join(filepath.Dir(root), "missing"), snapshot)
	// The inventory size gate runs before opening or decoding manifest bytes.
	require.Len(t, diagnostics, 1)
	require.Equal(t, "manifest-oversized", diagnostics[0].Code)
}

func TestTrustStoreRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), trustFilename)
	for _, data := range []string{
		`{"version":1,"records":{},"unknown":true}`,
		`{"version":1,"records":{}} {}`,
	} {
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
		_, err := loadTrustStore(path)
		require.Error(t, err)
	}
}

func TestCanonicalDigestIncludesFileMode(t *testing.T) {
	data := []bundleFile{{Path: "instructions/native.txt", Size: 1, Mode: 0o600, SHA256: "00"}}
	first := canonicalBundleDigest(data)
	data[0].Mode = 0o700
	require.NotEqual(t, first, canonicalBundleDigest(data))
}

func TestInstallRejectsUnsupportedLocalSources(t *testing.T) {
	manager := newTestManager(t)
	archive := filepath.Join(t.TempDir(), "bundle.zip")
	require.NoError(t, os.WriteFile(archive, []byte("not an archive source"), 0o600))

	tests := []struct {
		name    string
		request InstallRequest
		want    string
	}{
		{name: "empty", request: InstallRequest{}, want: "source is required"},
		{name: "archive file", request: InstallRequest{Source: archive}, want: "source root"},
		{name: "missing path", request: InstallRequest{Source: filepath.Join(t.TempDir(), "missing")}, want: "source root"},
		{name: "insecure remote", request: InstallRequest{Source: "http://example.invalid/plugin.git"}, want: "HTTPS Git"},
		{name: "local ref", request: InstallRequest{Source: exampleBundle(t, "minimal.plugin"), Ref: "main"}, want: "only for an HTTPS Git source"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := manager.Install(t.Context(), test.request)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestInstallRejectsSourceRootAndNestedSymlinks(t *testing.T) {
	manager := newTestManager(t)
	root := t.TempDir()
	sourceLink := filepath.Join(root, "source.plugin")
	require.NoError(t, os.Symlink(exampleBundle(t, "minimal.plugin"), sourceLink))
	_, err := manager.Install(t.Context(), InstallRequest{Source: sourceLink})
	require.ErrorContains(t, err, "source root")

	for _, target := range []string{exampleBundle(t, "minimal.plugin"), filepath.Join(root, "missing")} {
		source := t.TempDir()
		data, readErr := os.ReadFile(filepath.Join(exampleBundle(t, "minimal.plugin"), manifestFilename))
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(filepath.Join(source, manifestFilename), data, 0o600))
		require.NoError(t, os.Symlink(target, filepath.Join(source, "linked")))
		_, err = manager.Install(t.Context(), InstallRequest{Source: source})
		require.Error(t, err)
	}
}

func TestInstallRejectsCaseCollidingPathsAndExcessiveDepth(t *testing.T) {
	manager := newTestManager(t)

	collision := t.TempDir()
	data, err := os.ReadFile(filepath.Join(exampleBundle(t, "minimal.plugin"), manifestFilename))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(collision, manifestFilename), data, 0o600))
	upper := filepath.Join(collision, "Policy.txt")
	lower := filepath.Join(collision, "policy.txt")
	require.NoError(t, os.WriteFile(upper, []byte("one"), 0o600))
	if err := os.WriteFile(lower, []byte("two"), 0o600); err == nil {
		upperInfo, upperErr := os.Lstat(upper)
		lowerInfo, lowerErr := os.Lstat(lower)
		require.NoError(t, upperErr)
		require.NoError(t, lowerErr)
		if !os.SameFile(upperInfo, lowerInfo) {
			_, err = manager.Install(t.Context(), InstallRequest{Source: collision})
			require.ErrorContains(t, err, "collide case-insensitively")
		}
	}

	deep := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(deep, manifestFilename), data, 0o600))
	path := deep
	for i := 0; i <= MaxBundleDepth; i++ {
		path = filepath.Join(path, "d")
		require.NoError(t, os.Mkdir(path, 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(path, "value.txt"), []byte("deep"), 0o600))
	_, err = manager.Install(t.Context(), InstallRequest{Source: deep})
	require.ErrorContains(t, err, "exceeds host limits")
}

func TestInstallRejectsOversizedStaticText(t *testing.T) {
	manager := newTestManager(t)
	source := t.TempDir()
	value := readExampleManifest(t)
	value.Capabilities.Instructions = &manifest.InstructionPolicy{
		Profiles: map[string]string{"native": "instructions/native.txt"},
		Default:  "native",
	}
	writeBundleManifest(t, source, value)
	require.NoError(t, os.MkdirAll(filepath.Join(source, "instructions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "instructions", "native.txt"), make([]byte, MaxStaticTextBytes+1), 0o600))
	_, err := manager.Install(t.Context(), InstallRequest{Source: source})
	require.ErrorContains(t, err, "exceeds")
}

func TestDiagnosticScrubbingAndBound(t *testing.T) {
	diagnostic := safeDiagnostic("test", "authorization=secret Bearer token https://user:pass@example.invalid/path?token=x unsafe\x00\x01\n"+string(make([]byte, MaxDiagnosticBytes+50)))
	require.NotContains(t, diagnostic.Message, "\x00")
	require.NotContains(t, diagnostic.Message, "secret")
	require.NotContains(t, diagnostic.Message, "user:pass")
	require.Contains(t, diagnostic.Message, "<redacted>")
	require.Contains(t, diagnostic.Message, "<redacted-url>")
	require.LessOrEqual(t, len(diagnostic.Message), MaxDiagnosticBytes+len("…"))
	_, err := json.Marshal(diagnostic)
	require.NoError(t, err)
}
