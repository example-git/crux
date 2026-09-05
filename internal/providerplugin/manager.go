package providerplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/google/uuid"
)

const (
	MaxInstalledPlugins = 256
	managerLockTimeout  = 10 * time.Second
)

var (
	ErrStaleRevision = errors.New("plugin registry revision changed")
	ErrPluginMissing = errors.New("plugin not found")
)

// Manager owns host-global discovery, immutable installation, trust, and
// revisioned status. It never searches PATH or project directories.
type Manager struct {
	paths      Paths
	mu         sync.RWMutex
	state      Snapshot
	trust      trustStore
	provenance provenanceStore
	events     *pubsub.Broker[Event]
}

// NewManager initializes private storage, loads trust and provenance, and
// performs a full secure rescan before exposing any plugin state. Do not create
// a manager that skips Rescan or treats files present on disk as registered.
func NewManager(ctx context.Context, paths Paths) (*Manager, error) {
	if err := initializePaths(paths); err != nil {
		return nil, err
	}
	trust, err := loadTrustStore(paths.TrustFile)
	if err != nil {
		return nil, err
	}
	provenance, err := loadProvenance(paths.ProvenanceFile)
	if err != nil {
		return nil, err
	}
	manager := &Manager{paths: paths, trust: trust, provenance: provenance, events: pubsub.NewBroker[Event]()}
	if _, err := manager.Rescan(ctx, 0); err != nil {
		manager.events.Shutdown()
		return nil, err
	}
	return manager, nil
}

// initializePaths requires absolute host-owned directories for bundles, cache,
// and state. Relative or plugin-controlled roots would make trust and snapshot
// checks depend on process working directory.
func initializePaths(paths Paths) error {
	for label, path := range map[string]string{"plugin directory": paths.Bundles, "plugin cache": paths.Cache, "plugin state": paths.State} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be absolute", label)
		}
		if err := ensurePrivateDirectory(path); err != nil {
			return fmt.Errorf("initialize %s: %w", label, err)
		}
	}
	return nil
}

// ensurePrivateDirectory creates a non-symlink directory with private
// permissions. Do not relax either condition: installed manifests can contain
// private OAuth identity and endpoint policy.
func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is not a non-symlink directory")
	}
	return os.Chmod(path, 0o700)
}

// Close terminates the manager event broker and must remain paired with every
// manager lifecycle to avoid leaking subscribers.
func (m *Manager) Close() { m.events.Shutdown() }

// Subscribe publishes immutable revision notifications only; consumers must
// read a fresh Snapshot rather than retaining mutable internal plugin state.
func (m *Manager) Subscribe(ctx context.Context) <-chan pubsub.Event[Event] {
	return m.events.Subscribe(ctx)
}

// Snapshot returns a detached point-in-time view. Returning internal state
// would let UI or configuration code mutate trust and registration status.
func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state.clone()
}

// RegisteredBundle contains one trusted manifest and immutable copies of its
// declared static UTF-8 files. It never exposes an installed filesystem path.
type RegisteredBundle struct {
	Manifest   manifest.Manifest
	StaticText map[string]string
}

// RegisteredBundles is the only handoff from installed files to active
// provider-plugin registration. It returns deep copies only for exact-digest
// trusted, compatible, non-quarantined bundles and never exposes bundle paths.
// Do not include discovered, untrusted, invalid, or stale bytes to make a
// provider appear available.
func (m *Manager) RegisteredBundles() []RegisteredBundle {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]RegisteredBundle, 0, len(m.state.Plugins))
	for _, status := range m.state.Plugins {
		if status.State != StateRegistered || status.manifest == nil {
			continue
		}
		data, err := json.Marshal(status.manifest)
		if err != nil {
			continue
		}
		var value manifest.Manifest
		if json.Unmarshal(data, &value) == nil {
			result = append(result, RegisteredBundle{Manifest: value, StaticText: maps.Clone(status.staticText)})
		}
	}
	return result
}

type RegisteredPresetBundle struct {
	Manifest manifest.PresetManifest
	Digest   string
}

// RegisteredPresetBundles keeps data-only presets separate from provider
// plugins. A preset must never acquire OAuth, identity, transport, or executable
// registration behavior through this projection.
func (m *Manager) RegisteredPresetBundles() []RegisteredPresetBundle {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]RegisteredPresetBundle, 0, len(m.state.Plugins))
	for _, status := range m.state.Plugins {
		if status.State != StateRegistered || status.preset == nil {
			continue
		}
		data, err := json.Marshal(status.preset)
		if err != nil {
			continue
		}
		var value manifest.PresetManifest
		if json.Unmarshal(data, &value) == nil {
			result = append(result, RegisteredPresetBundle{Manifest: value, Digest: status.Digest})
		}
	}
	return result
}

// RegisteredManifests retains the manifest-only projection for legacy catalog
// callers. Runtime registration should use RegisteredBundles so validated
// static instruction text is not discarded.
func (m *Manager) RegisteredManifests() []manifest.Manifest {
	bundles := m.RegisteredBundles()
	result := make([]manifest.Manifest, 0, len(bundles))
	for _, bundle := range bundles {
		result = append(result, bundle.Manifest)
	}
	return result
}

// Rescan atomically rebuilds plugin state from canonical bytes, current exact-
// digest trust, provenance, compatibility, and duplicate policy. Keep the file
// lock, revision check, and state swap together; partial refresh can pair trust
// from one bundle version with manifest bytes from another.
func (m *Manager) Rescan(ctx context.Context, expectedRevision uint64) (Snapshot, error) {
	lockContext, cancel := context.WithTimeout(ctx, managerLockTimeout)
	defer cancel()
	release, err := lock.File(lockContext, m.paths.ManagerLock)
	if err != nil {
		return Snapshot{}, fmt.Errorf("lock plugin registry: %w", err)
	}
	defer release()

	m.mu.Lock()
	defer m.mu.Unlock()
	if expectedRevision != 0 && expectedRevision != m.state.Revision {
		return Snapshot{}, ErrStaleRevision
	}
	trust, err := loadTrustStore(m.paths.TrustFile)
	if err != nil {
		return Snapshot{}, err
	}
	provenance, err := loadProvenance(m.paths.ProvenanceFile)
	if err != nil {
		return Snapshot{}, err
	}
	plugins, err := m.scanLocked(ctx, trust, provenance)
	if err != nil {
		return Snapshot{}, err
	}
	m.applyDuplicatePolicy(plugins)
	slices.SortFunc(plugins, func(a, b Status) int { return strings.Compare(a.BundleName, b.BundleName) })
	m.trust = trust
	m.provenance = provenance
	m.state = Snapshot{Revision: m.state.Revision + 1, ScannedAt: time.Now().UTC(), Plugins: plugins}
	result := m.state.clone()
	m.events.Publish(pubsub.UpdatedEvent, Event{Revision: result.Revision, Kind: "rescanned"})
	return result, nil
}

// scanLocked snapshots each direct non-symlink bundle before validation and
// derives registration eligibility from those immutable bytes. Never parse an
// installed bundle in place or trust by ID, path, publisher, or previous state;
// only the validated canonical digest may become registered.
func (m *Manager) scanLocked(ctx context.Context, trust trustStore, provenance provenanceStore) ([]Status, error) {
	directory, err := os.Open(m.paths.Bundles)
	if err != nil {
		return nil, fmt.Errorf("open canonical plugin directory: %w", err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(MaxInstalledPlugins + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read canonical plugin directory: %w", err)
	}
	if len(entries) > MaxInstalledPlugins {
		return nil, fmt.Errorf("canonical plugin directory exceeds %d direct entries", MaxInstalledPlugins)
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	seenNames := map[string]int{}
	statuses := make([]Status, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if !strings.HasSuffix(name, bundleSuffix) {
			continue
		}
		status := Status{BundleName: name, State: StateDiscovered, Trust: TrustUnknown, Compatibility: CompatibilityUnknown}
		folded := strings.ToLower(name)
		if prior, ok := seenNames[folded]; ok {
			message := fmt.Sprintf("bundle names %q and %q collide case-insensitively", statuses[prior].BundleName, name)
			statuses[prior].State = StateQuarantined
			statuses[prior].Diagnostics = append(statuses[prior].Diagnostics, safeDiagnostic("bundle-name-collision", message))
			status.State = StateQuarantined
			status.Diagnostics = []Diagnostic{safeDiagnostic("bundle-name-collision", message)}
			statuses = append(statuses, status)
			continue
		}
		seenNames[folded] = len(statuses)
		info, err := entry.Info()
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !validEntryName(strings.TrimSuffix(name, bundleSuffix)) {
			status.State = StateInvalid
			status.Diagnostics = []Diagnostic{safeDiagnostic("bundle-entry-unsafe", "installed plugin entry is not a safe direct directory")}
			statuses = append(statuses, status)
			continue
		}
		status.InstalledAt = info.ModTime().UTC()
		temporary := filepath.Join(m.paths.Cache, ".scan-"+uuid.NewString())
		snapshot, err := snapshotDirectory(filepath.Join(m.paths.Bundles, name), temporary)
		if err != nil {
			_ = os.RemoveAll(temporary)
			status.State = StateInvalid
			status.Diagnostics = []Diagnostic{safeDiagnostic("bundle-snapshot-invalid", "installed plugin bundle could not be read securely")}
			statuses = append(statuses, status)
			continue
		}
		validated, diagnostics := validateSnapshot(temporary, snapshot)
		_ = os.RemoveAll(temporary)
		if len(diagnostics) > 0 {
			status.State = StateInvalid
			status.Digest = snapshot.Digest
			status.Diagnostics = diagnostics
			statuses = append(statuses, status)
			continue
		}
		status.PluginType = validated.pluginType
		status.ID = validated.id()
		status.ProviderID = validated.providerID()
		status.Name = validated.name()
		status.Version = validated.version()
		status.PublisherID = validated.publisherID()
		status.Digest = validated.digest
		status.Capabilities = validated.capabilityIDs()
		if record, ok := provenance.Records[validated.digest]; ok && record.PluginID == status.ID && record.Digest == validated.digest {
			status.SourceKind = record.SourceKind
			status.SourceCommit = record.Commit
			status.InstalledAt = record.Installed
		}
		status.manifest = validated.manifest
		status.preset = validated.preset
		status.image = validated.image
		status.staticText = maps.Clone(validated.staticText)
		status.path = filepath.Join(m.paths.Bundles, name)
		if name != status.ID+bundleSuffix {
			status.State = StateInvalid
			status.Diagnostics = []Diagnostic{safeDiagnostic("bundle-name-mismatch", fmt.Sprintf("bundle directory must be named %s%s", status.ID, bundleSuffix))}
			statuses = append(statuses, status)
			continue
		}
		compatibility := compatibilityDiagnostics(validated.compatibility())
		if len(compatibility) > 0 {
			status.State = StateIncompatible
			status.Compatibility = CompatibilityIncompatible
			status.Diagnostics = compatibility
			statuses = append(statuses, status)
			continue
		}
		status.Compatibility = CompatibilityCompatible
		status.Trust = trust.state(validated)
		if status.Trust != TrustTrusted {
			status.State = StateUntrusted
			if status.Trust == TrustRevoked {
				status.State = StateQuarantined
				status.Diagnostics = []Diagnostic{safeDiagnostic("trust-revoked", "trust for this exact plugin digest was revoked")}
			}
		} else {
			applyTrustedRegistrationPolicy(&status)
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// applyDuplicatePolicy quarantines ambiguous duplicate plugin or provider
// ownership while preserving an exact canonical migrated preset. Canonical
// selection uses its built-in identity and digest, never scan order.
func (m *Manager) applyDuplicatePolicy(statuses []Status) {
	pluginIDs := map[string][]int{}
	providerIDs := map[string][]int{}
	imageIDs := map[string][]int{}
	for i, status := range statuses {
		if status.ID != "" {
			pluginIDs[status.ID] = append(pluginIDs[status.ID], i)
		}
		if status.ProviderID != "" {
			if status.PluginType == manifest.PluginTypeImageProvider {
				imageIDs[status.ProviderID] = append(imageIDs[status.ProviderID], i)
			} else {
				providerIDs[status.ProviderID] = append(providerIDs[status.ProviderID], i)
			}
		}
	}
	quarantineOne := func(index int, code, message string) {
		statuses[index].State = StateQuarantined
		statuses[index].Diagnostics = append(statuses[index].Diagnostics, safeDiagnostic(code, message))
	}
	quarantine := func(indexes []int, code, message string) {
		if len(indexes) < 2 {
			return
		}
		for _, index := range indexes {
			quarantineOne(index, code, message)
		}
	}
	for id, indexes := range pluginIDs {
		quarantine(indexes, "duplicate-plugin-id", fmt.Sprintf("multiple bundles claim plugin ID %q", id))
	}
	for id, indexes := range imageIDs {
		quarantine(indexes, "duplicate-image-backend", fmt.Sprintf("multiple bundles claim image backend ID %q", id))
	}
	for id, indexes := range providerIDs {
		if len(indexes) < 2 {
			continue
		}
		canonical := -1
		for _, index := range indexes {
			status := statuses[index]
			if status.preset == nil || !IsCanonicalMigratedProviderPresetBundle(id, status.preset.ID, status.preset.Version, status.Digest) {
				continue
			}
			if canonical >= 0 {
				canonical = -1
				break
			}
			canonical = index
		}
		if canonical < 0 {
			quarantine(indexes, "duplicate-provider-id", fmt.Sprintf("multiple bundles claim provider ID %q", id))
			continue
		}
		for _, index := range indexes {
			if index == canonical {
				continue
			}
			if statuses[index].preset != nil {
				quarantineOne(index, "migrated-preset-canonical-mismatch", "installed migrated provider preset does not match the canonical bundled version")
				continue
			}
			quarantineOne(index, "migrated-provider-plugin-conflict", "provider plugin cannot claim an ID reserved for its canonical migrated preset")
		}
	}
}
