package providerplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

// TransportBundle carries private declarative bytes, not installation paths or
// server trust requests. Files retain their original bytes for digest identity.
// HTTP callers must bound the encoded envelope before decoding base64 content.
type TransportBundle struct {
	Digest string          `json:"digest"`
	Files  []TransportFile `json:"files"`
}

type TransportFile struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

// DetachedBundle holds validated, workspace-local content. No filesystem or
// global manager/trust operation is performed during construction or reads.
type DetachedBundle struct{ value validatedBundle }

func (b DetachedBundle) ID() string         { return b.value.id() }
func (b DetachedBundle) ProviderID() string { return b.value.providerID() }
func (b DetachedBundle) Version() string    { return b.value.version() }
func (b DetachedBundle) Type() string       { return b.value.pluginType }
func (b DetachedBundle) Digest() string     { return b.value.digest }

func (b DetachedBundle) Catalog() (catalog.Provider, error) {
	if b.value.manifest != nil {
		provider, err := catalogProvider(*b.value.manifest)
		if b.value.manifest.Provider.Brand != nil {
			brand := *b.value.manifest.Provider.Brand
			provider.Brand = &brand
		}
		return provider, err
	}
	if b.value.preset != nil {
		provider := catalogPreset(b.value.preset.Preset)
		if b.value.preset.Brand != nil {
			brand := *b.value.preset.Brand
			provider.Brand = &brand
		}
		return provider, nil
	}
	return catalog.Provider{}, errors.New("image bundles do not define inference catalogs")
}

func detachedCopy[T any](value T) T {
	data, err := json.Marshal(value)
	if err != nil {
		panic("validated bundle cannot be encoded")
	}
	var copy T
	if json.Unmarshal(data, &copy) != nil {
		panic("validated bundle cannot be copied")
	}
	return copy
}

func (b DetachedBundle) Provider() *RegisteredBundle {
	if b.value.manifest == nil {
		return nil
	}
	return detachedCopy(&RegisteredBundle{Manifest: *b.value.manifest, StaticText: b.value.staticText})
}

func (b DetachedBundle) Preset() *RegisteredPresetBundle {
	if b.value.preset == nil {
		return nil
	}
	return detachedCopy(&RegisteredPresetBundle{Manifest: *b.value.preset, Digest: b.value.digest})
}

func (b DetachedBundle) Image() *manifest.ImageManifest { return detachedCopy(b.value.image) }

func (b DetachedBundle) Export() TransportBundle { return exportValidatedBundle(b.value) }

func exportValidatedBundle(value validatedBundle) TransportBundle {
	result := TransportBundle{Digest: value.digest}
	for path, data := range value.rawFiles {
		result.Files = append(result.Files, TransportFile{Path: path, Data: slices.Clone(data)})
	}
	slices.SortFunc(result.Files, func(a, b TransportFile) int { return strings.Compare(a.Path, b.Path) })
	return result
}

func ValidateDetachedBundle(input TransportBundle) (DetachedBundle, error) {
	if len(input.Files) == 0 || len(input.Files) > MaxBundleFiles {
		return DetachedBundle{}, errors.New("bundle file count exceeds receiver limits")
	}
	if len(input.Digest) != sha256.Size*2 {
		return DetachedBundle{}, errors.New("invalid bundle digest")
	}
	files := make(map[string][]byte, len(input.Files))
	names := make(map[string]string, len(input.Files))
	dirs := make(map[string]string)
	snapshot := snapshotResult{}
	for _, file := range input.Files {
		parts := strings.Split(file.Path, "/")
		if !validRelativePath(file.Path, len(parts)) {
			return DetachedBundle{}, errors.New("invalid bundle-relative path")
		}
		for _, part := range parts {
			if !validEntryName(part) || strings.IndexFunc(part, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
				return DetachedBundle{}, errors.New("invalid bundle-relative path")
			}
		}
		lower := strings.ToLower(file.Path)
		if _, exists := names[lower]; exists {
			return DetachedBundle{}, errors.New("duplicate or case-colliding bundle path")
		}
		names[lower] = file.Path
		for index := 1; index < len(parts); index++ {
			dir := strings.Join(parts[:index], "/")
			key := strings.ToLower(dir)
			if prior, exists := dirs[key]; exists && prior != dir {
				return DetachedBundle{}, errors.New("case-colliding bundle directory")
			}
			dirs[key] = dir
		}
		size := int64(len(file.Data))
		if size > MaxFileBytes || snapshot.TotalBytes > MaxBundleBytes-size {
			return DetachedBundle{}, errors.New("bundle bytes exceed receiver limits")
		}
		snapshot.TotalBytes += size
		hash := sha256.Sum256(file.Data)
		snapshot.Files = append(snapshot.Files, bundleFile{Path: file.Path, Size: size, Mode: 0o600, SHA256: hex.EncodeToString(hash[:])})
		files[file.Path] = slices.Clone(file.Data)
	}
	if len(dirs) > MaxBundleDirectories {
		return DetachedBundle{}, errors.New("bundle directory count exceeds receiver limits")
	}
	for path := range dirs {
		if _, exists := names[path]; exists {
			return DetachedBundle{}, errors.New("bundle file collides with a directory")
		}
	}
	snapshot.Digest = canonicalBundleDigest(snapshot.Files)
	if input.Digest != snapshot.Digest {
		return DetachedBundle{}, errors.New("bundle digest mismatch")
	}
	value, diagnostics := validateBundleFiles(snapshot, func(path string, limit int64) ([]byte, error) {
		data, ok := files[path]
		if !ok || int64(len(data)) > limit {
			return nil, errors.New("missing or oversized bundle file")
		}
		return data, nil
	})
	if len(diagnostics) > 0 {
		return DetachedBundle{}, fmt.Errorf("received bundle rejected: %s", diagnostics[0].Code)
	}
	if diagnostics = compatibilityDiagnostics(value.compatibility()); len(diagnostics) > 0 {
		return DetachedBundle{}, fmt.Errorf("received bundle incompatible: %s", diagnostics[0].Code)
	}
	value.rawFiles = files
	return DetachedBundle{value: value}, nil
}

// ExportRegisteredBundles captures one exact manager generation. An explicitly
// selected missing/untrusted/replaced digest is an error, never a partial export.
func (m *Manager) ExportRegisteredBundles(revision uint64, selected map[string]string) ([]TransportBundle, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if revision != m.state.Revision {
		return nil, ErrStaleRevision
	}
	result := make([]TransportBundle, 0, len(selected))
	for id, digest := range selected {
		var found bool
		for _, status := range m.state.Plugins {
			if status.ID != id {
				continue
			}
			if status.State != StateRegistered || status.Digest != digest {
				return nil, errors.New("selected bundle is not its registered exact digest")
			}
			value, ok := m.validated[status.Digest]
			if !ok || value.digest != digest || len(value.rawFiles) == 0 {
				return nil, errors.New("selected bundle generation has no validated export")
			}
			result = append(result, exportValidatedBundle(value))
			found = true
			break
		}
		if !found {
			return nil, ErrPluginMissing
		}
	}
	slices.SortFunc(result, func(a, b TransportBundle) int { return strings.Compare(a.Digest, b.Digest) })
	return result, nil
}
