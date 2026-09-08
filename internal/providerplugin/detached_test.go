package providerplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func detachedFixture(t *testing.T, source string) TransportBundle {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "snapshot")
	snapshot, err := snapshotForValidation(source, destination)
	require.NoError(t, err)
	validated, diagnostics := validateSnapshot(destination, snapshot)
	require.Empty(t, diagnostics)
	return exportValidatedBundle(validated)
}

func withTransportDigest(bundle TransportBundle) TransportBundle {
	var files []bundleFile
	for _, file := range bundle.Files {
		hash := sha256.Sum256(file.Data)
		files = append(files, bundleFile{Path: file.Path, Size: int64(len(file.Data)), Mode: 0o600, SHA256: hex.EncodeToString(hash[:])})
	}
	bundle.Digest = canonicalBundleDigest(files)
	return bundle
}

func TestDetachedBundleMatchesInstalledValidationAndOriginalBytes(t *testing.T) {
	for _, name := range []string{"minimal.plugin", "responses-oauth.plugin", "deepseek-preset.plugin"} {
		t.Run(name, func(t *testing.T) {
			input := detachedFixture(t, exampleBundle(t, name))
			bundle, err := ValidateDetachedBundle(input)
			require.NoError(t, err)
			require.Equal(t, input, bundle.Export())
			require.Equal(t, input.Digest, bundle.Digest())
			if name == "deepseek-preset.plugin" {
				require.NotNil(t, bundle.Preset())
				require.Nil(t, bundle.Provider())
			} else {
				require.NotNil(t, bundle.Provider())
				require.Nil(t, bundle.Preset())
			}
			saved := bundle.Export()
			input.Files[0].Data[0] = 'x'
			require.Equal(t, saved, bundle.Export(), "caller input mutation must not affect accepted bytes")
			exported := bundle.Export()
			exported.Files[0].Data[0] = 'x'
			require.Equal(t, saved, bundle.Export(), "export mutation must not affect later captures")
			if provider := bundle.Provider(); provider != nil {
				provider.Manifest.Name = "changed"
				for path := range provider.StaticText {
					provider.StaticText[path] = "changed"
				}
				require.NotEqual(t, "changed", bundle.Provider().Manifest.Name)
			}
		})
	}
}

func TestDetachedBundleRejectsUnsafeReceivedContent(t *testing.T) {
	base := detachedFixture(t, exampleBundle(t, "minimal.plugin"))
	for _, test := range []struct {
		name           string
		change         func(*TransportBundle)
		message        string
		preserveDigest bool
	}{
		{"digest", func(b *TransportBundle) { b.Digest = strings.Repeat("0", 64) }, "digest mismatch", true},
		{"traversal", func(b *TransportBundle) { b.Files[0].Path = "../manifest.json" }, "bundle-relative path", false},
		{"absolute", func(b *TransportBundle) { b.Files[0].Path = "/manifest.json" }, "bundle-relative path", false},
		{"windows", func(b *TransportBundle) { b.Files[0].Path = `C:\manifest.json` }, "bundle-relative path", false},
		{"control", func(b *TransportBundle) { b.Files[0].Path = "secret\nmanifest.json" }, "bundle-relative path", false},
		{"duplicate", func(b *TransportBundle) { b.Files = append(b.Files, b.Files[0]) }, "duplicate", false},
		{"case", func(b *TransportBundle) {
			b.Files = append(b.Files, TransportFile{Path: "MANIFEST.JSON", Data: b.Files[0].Data})
		}, "case-colliding", false},
		{"directory-case", func(b *TransportBundle) {
			b.Files = append(b.Files, TransportFile{Path: "instructions/a"}, TransportFile{Path: "Instructions/b"})
		}, "case-colliding bundle directory", false},
		{"file-directory", func(b *TransportBundle) { b.Files = append(b.Files, TransportFile{Path: "manifest.json/extra"}) }, "file collides", false},
		{"undeclared", func(b *TransportBundle) {
			b.Files = append(b.Files, TransportFile{Path: "execute.sh", Data: []byte("private-executable-content")})
		}, "bundle-file-unexpected", false},
		{"malformed", func(b *TransportBundle) { b.Files[0].Data = []byte(`{"private-secret-input":`) }, "manifest-invalid", false},
		{"too-many", func(b *TransportBundle) { b.Files = make([]TransportFile, MaxBundleFiles+1) }, "file count", false},
		{"depth", func(b *TransportBundle) { b.Files[0].Path = strings.Repeat("dir/", MaxBundleDepth) + "manifest.json" }, "bundle-relative path", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := detachedCopy(base)
			test.change(&input)
			if !test.preserveDigest {
				input = withTransportDigest(input)
			}
			_, err := ValidateDetachedBundle(input)
			require.ErrorContains(t, err, test.message)
			require.NotContains(t, err.Error(), "private-secret-input")
			require.NotContains(t, err.Error(), "private-executable-content")
		})
	}
}

func TestDetachedBundleRejectsIncompatibleHost(t *testing.T) {
	input := detachedFixture(t, exampleBundle(t, "minimal.plugin"))
	for index := range input.Files {
		if input.Files[index].Path != manifestFilename {
			continue
		}
		var value map[string]any
		require.NoError(t, json.Unmarshal(input.Files[index].Data, &value))
		value["compatibility"].(map[string]any)["host_api"] = map[string]any{"min": 999, "max": 999}
		data, err := json.Marshal(value)
		require.NoError(t, err)
		input.Files[index].Data = data
	}
	_, err := ValidateDetachedBundle(withTransportDigest(input))
	require.ErrorContains(t, err, "host-api-incompatible")
}

func TestExportRegisteredBundlesCapturesExactGenerationWithoutReread(t *testing.T) {
	manager := newTestManager(t)
	snapshot, err := manager.Install(t.Context(), InstallRequest{Source: exampleBundle(t, "responses-oauth.plugin")})
	require.NoError(t, err)
	status := snapshot.Plugins[0]
	snapshot, err = manager.SetTrust(t.Context(), status.ID, TrustRequest{Digest: status.Digest, Trusted: true})
	require.NoError(t, err)
	selected := map[string]string{status.ID: status.Digest}
	first, err := manager.ExportRegisteredBundles(snapshot.Revision, selected)
	require.NoError(t, err)
	require.Len(t, first, 1)
	_, err = ValidateDetachedBundle(first[0])
	require.NoError(t, err)
	// Changing installation bytes after capture cannot change an already
	// accepted generation or cause an arbitrary pathname reread during export.
	require.NoError(t, os.WriteFile(filepath.Join(manager.paths.Bundles, status.BundleName, manifestFilename), []byte("changed after capture"), 0o600))
	again, err := manager.ExportRegisteredBundles(snapshot.Revision, selected)
	require.NoError(t, err)
	require.Equal(t, first, again)
	first[0].Files[0].Data[0] = 'x'
	third, err := manager.ExportRegisteredBundles(snapshot.Revision, selected)
	require.NoError(t, err)
	require.Equal(t, again, third)
	_, err = manager.ExportRegisteredBundles(snapshot.Revision+1, selected)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = manager.Rescan(t.Context(), snapshot.Revision)
	require.NoError(t, err)
	_, err = manager.ExportRegisteredBundles(manager.Snapshot().Revision, selected)
	require.Error(t, err, "rescan invalidation must prevent exporting replaced bytes")
}

// Explicit local acceptance input; never commits private bundle contents.
func TestDetachedBundleUserFixtures(t *testing.T) {
	roots := os.Getenv("CRUX_REMOTE_BUNDLE_FIXTURES")
	if roots == "" {
		t.Skip("set CRUX_REMOTE_BUNDLE_FIXTURES to explicit local bundle roots")
	}
	var sources []string
	for _, root := range filepath.SplitList(roots) {
		matches, err := filepath.Glob(filepath.Join(root, "*.plugin"))
		require.NoError(t, err)
		sources = append(sources, matches...)
	}
	require.NotEmpty(t, sources)
	slices.Sort(sources)
	for _, source := range sources {
		t.Run(filepath.Base(source), func(t *testing.T) {
			input := detachedFixture(t, source)
			bundle, err := ValidateDetachedBundle(input)
			require.NoError(t, err)
			require.Equal(t, input, bundle.Export())
		})
	}
}
