package providerplugin

import (
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/version"
	"golang.org/x/mod/semver"
)

var supportedFeatures = map[string]struct{}{
	"continuation.previous-response":    {},
	"transport.anthropic-messages-http": {},
	"transport.gemini-generate-content": {},
	"transport.openai-responses-http":   {},
}

type validatedBundle struct {
	pluginType string
	manifest   *manifest.Manifest
	preset     *manifest.PresetManifest
	image      *manifest.ImageManifest
	files      map[string]bundleFile
	staticText map[string]string
	digest     string
}

func (b validatedBundle) id() string {
	if b.manifest != nil {
		return b.manifest.ID
	}
	if b.image != nil {
		return b.image.ID
	}
	return b.preset.ID
}

func (b validatedBundle) providerID() string {
	if b.manifest != nil {
		return b.manifest.Provider.ID
	}
	if b.image != nil {
		return b.image.Backend
	}
	return string(b.preset.Preset.ID)
}

func (b validatedBundle) name() string {
	if b.manifest != nil {
		return b.manifest.Name
	}
	if b.image != nil {
		return b.image.Name
	}
	return b.preset.Name
}

func (b validatedBundle) version() string {
	if b.manifest != nil {
		return b.manifest.Version
	}
	if b.image != nil {
		return b.image.Version
	}
	return b.preset.Version
}

func (b validatedBundle) publisherID() string {
	if b.manifest != nil {
		return b.manifest.Publisher.ID
	}
	if b.image != nil {
		return b.image.Publisher.ID
	}
	return b.preset.Publisher.ID
}

func (b validatedBundle) compatibility() manifest.Compatibility {
	if b.manifest != nil {
		return b.manifest.Compatibility
	}
	if b.image != nil {
		return b.image.Compatibility
	}
	return b.preset.Compatibility
}

func (b validatedBundle) capabilityIDs() []string {
	if b.manifest != nil {
		return capabilityIDs(*b.manifest)
	}
	if b.image != nil {
		capabilities := []string{"image-generate"}
		if b.image.Edit != "" {
			capabilities = append(capabilities, "image-edit")
		}
		return capabilities
	}
	return []string{"provider-preset"}
}

func validateSnapshot(root string, snapshot snapshotResult) (validatedBundle, []Diagnostic) {
	files := make(map[string]bundleFile, len(snapshot.Files))
	for _, file := range snapshot.Files {
		files[file.Path] = file
	}
	manifestFile, ok := files[manifestFilename]
	if !ok {
		return validatedBundle{}, []Diagnostic{safeDiagnostic("manifest-missing", "bundle root does not contain manifest.json")}
	}
	if manifestFile.Size > manifest.MaxManifestBytes {
		return validatedBundle{}, []Diagnostic{safeDiagnostic("manifest-oversized", fmt.Sprintf("manifest.json exceeds %d bytes", manifest.MaxManifestBytes))}
	}
	data, err := readBoundedRegularFile(filepath.Join(root, manifestFilename), manifest.MaxManifestBytes)
	if err != nil {
		return validatedBundle{}, []Diagnostic{safeDiagnostic("manifest-read-failed", err.Error())}
	}
	pluginType, err := manifest.DecodePluginType(data)
	if err != nil {
		return validatedBundle{}, []Diagnostic{safeDiagnostic("manifest-invalid", err.Error())}
	}

	validated := validatedBundle{
		pluginType: pluginType,
		files:      files,
		staticText: make(map[string]string),
		digest:     snapshot.Digest,
	}
	referenced := map[string]struct{}{manifestFilename: {}}
	switch pluginType {
	case manifest.PluginTypeProvider:
		value, err := manifest.DecodeStrict(data)
		if err != nil {
			return validatedBundle{}, []Diagnostic{safeDiagnostic("manifest-invalid", err.Error())}
		}
		validated.manifest = &value
		if value.Capabilities.Instructions != nil {
			for _, path := range value.Capabilities.Instructions.Profiles {
				referenced[path] = struct{}{}
				file, ok := files[path]
				if !ok {
					return validatedBundle{}, []Diagnostic{safeDiagnostic("bundle-path-missing", fmt.Sprintf("manifest references missing file %q", path))}
				}
				if file.Size > MaxStaticTextBytes {
					return validatedBundle{}, []Diagnostic{safeDiagnostic("static-text-oversized", fmt.Sprintf("declared static text %q exceeds %d bytes", path, MaxStaticTextBytes))}
				}
				data, err := readBoundedRegularFile(filepath.Join(root, filepath.FromSlash(path)), MaxStaticTextBytes)
				if err != nil || !utf8.Valid(data) {
					return validatedBundle{}, []Diagnostic{safeDiagnostic("static-text-invalid", fmt.Sprintf("declared static text %q must be bounded UTF-8", path))}
				}
				validated.staticText[path] = string(data)
			}
		}
	case manifest.PluginTypeProviderPreset:
		value, err := manifest.DecodePresetStrict(data)
		if err != nil {
			return validatedBundle{}, []Diagnostic{safeDiagnostic("manifest-invalid", err.Error())}
		}
		validated.preset = &value
	case manifest.PluginTypeImageProvider:
		value, err := manifest.DecodeImageStrict(data)
		if err != nil {
			return validatedBundle{}, []Diagnostic{safeDiagnostic("manifest-invalid", err.Error())}
		}
		validated.image = &value
	default:
		return validatedBundle{}, []Diagnostic{safeDiagnostic("manifest-invalid", fmt.Sprintf("unsupported plugin type %q", pluginType))}
	}
	for path := range files {
		if _, ok := referenced[path]; !ok {
			return validatedBundle{}, []Diagnostic{safeDiagnostic("bundle-file-unexpected", fmt.Sprintf("bundle contains undeclared file %q", path))}
		}
	}
	return validated, nil
}

func readBoundedRegularFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum {
		return nil, fmt.Errorf("manifest is not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("manifest exceeds %d bytes", maximum)
	}
	return data, nil
}

func compatibilityDiagnostics(value manifest.Compatibility) []Diagnostic {
	var diagnostics []Diagnostic
	bounds := value.HostAPI
	if manifest.HostAPIVersion < bounds.Min || manifest.HostAPIVersion > bounds.Max {
		diagnostics = append(diagnostics, safeDiagnostic("host-api-incompatible", fmt.Sprintf("plugin requires host API %d through %d; host provides %d", bounds.Min, bounds.Max, manifest.HostAPIVersion)))
	}
	for _, feature := range value.RequiredFeatures {
		if _, ok := supportedFeatures[feature]; !ok {
			diagnostics = append(diagnostics, safeDiagnostic("host-feature-unsupported", fmt.Sprintf("required host feature %q is unsupported", feature)))
		}
	}
	if bounds := value.HostVersion; bounds != nil {
		host := strings.TrimPrefix(version.Version, "v")
		if !semver.IsValid("v" + host) {
			diagnostics = append(diagnostics, safeDiagnostic("host-version-unavailable", "host release version is unavailable for this plugin's release constraint"))
		} else if bounds.Min != "" && semver.Compare("v"+host, "v"+bounds.Min) < 0 {
			diagnostics = append(diagnostics, safeDiagnostic("host-version-too-old", fmt.Sprintf("plugin requires host version %s or newer", bounds.Min)))
		} else if bounds.Max != "" && semver.Compare("v"+host, "v"+bounds.Max) > 0 {
			diagnostics = append(diagnostics, safeDiagnostic("host-version-too-new", fmt.Sprintf("plugin supports host versions through %s", bounds.Max)))
		}
	}
	return diagnostics
}

func capabilityIDs(value manifest.Manifest) []string {
	var result []string
	c := value.Capabilities
	add := func(enabled bool, value string) {
		if enabled {
			result = append(result, value)
		}
	}
	add(len(c.Credentials) > 0, "credentials")
	add(len(c.OAuth) > 0, "oauth")
	add(len(c.Endpoints) > 0, "endpoints")
	add(len(c.Operations) > 0, "operations")
	add(c.Usage != nil, "usage")
	add(c.Images != nil, "images")
	add(c.Instructions != nil, "instructions")
	add(len(c.RuntimeControls) > 0, "runtime-controls")
	add(len(c.Metadata) > 0, "metadata")
	add(len(c.Errors) > 0, "errors")
	slices.Sort(result)
	return result
}

func equalDigest(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
