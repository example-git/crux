package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
)

type ProviderProfile string

const (
	ProviderProfileCoreOnly     ProviderProfile = "core-only"
	ProviderProfileIntegrated   ProviderProfile = "integrated"
	ProviderProfilePluginCompat ProviderProfile = "plugin-compat"
	ProviderProfilePluginNative ProviderProfile = "plugin-native"
)

// DefaultProviderProfile is a release-time ceiling and may be set with -ldflags
// -X. The host environment can select another supported profile only when the
// release uses the default compatibility profile.
var DefaultProviderProfile = string(ProviderProfilePluginCompat)

type providerRolloutPolicy struct {
	Profile         ProviderProfile
	Enabled         map[string]bool
	ExplicitProfile bool
}

var (
	providerStateMu          sync.RWMutex
	providerOnce             sync.Once
	providerList             []catalog.Provider
	providerRegistry         *providerregistry.Registry
	providerPluginStatuses   map[string]providerplugin.Status
	providerPresetReferences map[string]ProviderPresetReference
	providerOwnerModes       map[string]providerregistry.OwnerMode
	providerErr              error
)

const providerScanTimeout = 45 * time.Second

type ProviderScan struct {
	Providers []catalog.Provider
	Registry  *providerregistry.Registry

	pluginStatuses   map[string]providerplugin.Status
	presetReferences map[string]ProviderPresetReference
	ownerModes       map[string]providerregistry.OwnerMode
}

func cloneProviderScan(scan ProviderScan) ProviderScan {
	result := ProviderScan{
		Providers:        cloneProviderCatalog(scan.Providers),
		pluginStatuses:   cloneProviderStatuses(scan.pluginStatuses),
		presetReferences: maps.Clone(scan.presetReferences),
		ownerModes:       maps.Clone(scan.ownerModes),
	}
	if scan.Registry != nil {
		result.Registry = scan.Registry.Clone()
	}
	return result
}

func cloneProviderStatuses(statuses map[string]providerplugin.Status) map[string]providerplugin.Status {
	result := make(map[string]providerplugin.Status, len(statuses))
	for id, status := range statuses {
		result[id] = status.Clone()
	}
	return result
}

func (c *Config) bindProviderScan(scan ProviderScan) {
	bound := cloneProviderScan(scan)
	c.providerScan = &bound
}

func (c *Config) providerCapabilities() *providerregistry.Registry {
	if c == nil {
		return ProviderCapabilities()
	}
	if c.providerScan == nil || c.providerScan.Registry == nil {
		return nil
	}
	return c.providerScan.Registry
}

func (c *Config) activeProviderPreset(providerID string) (ProviderPresetReference, bool) {
	if c != nil && c.providerScan != nil {
		reference, found := c.providerScan.presetReferences[providerID]
		return reference, found
	}
	return ActiveProviderPreset(providerID)
}

func (c *Config) providerPluginAvailability(pluginID string) (providerplugin.Status, providerregistry.OwnerMode, bool) {
	if c != nil && c.providerScan != nil {
		status, found := c.providerScan.pluginStatuses[pluginID]
		return status.Clone(), c.providerScan.ownerModes[status.ProviderID], found
	}
	return ProviderPluginAvailability(pluginID)
}

func currentProviderScan() (ProviderScan, bool) {
	providerStateMu.RLock()
	defer providerStateMu.RUnlock()
	if providerRegistry == nil {
		return ProviderScan{}, false
	}
	return cloneProviderScan(ProviderScan{
		Providers:        providerList,
		Registry:         providerRegistry,
		pluginStatuses:   providerPluginStatuses,
		presetReferences: providerPresetReferences,
		ownerModes:       providerOwnerModes,
	}), true
}

func publishProviderScan(scan ProviderScan, publish func(ProviderScan)) {
	_ = publishProviderScanGeneration(scan, func(published ProviderScan) error {
		if publish != nil {
			publish(published)
		}
		return nil
	}, false)
}

func publishConfiguredProviderScan(scan ProviderScan, publish func(ProviderScan) error) error {
	return publishProviderScanGeneration(scan, publish, true)
}

func publishProviderScanGeneration(scan ProviderScan, publish func(ProviderScan) error, markInitialized bool) error {
	published := cloneProviderScan(scan)
	providerStateMu.Lock()
	defer providerStateMu.Unlock()
	if publish != nil {
		if err := publish(published); err != nil {
			return err
		}
	}
	if markInitialized {
		providerOnce.Do(func() {})
	}
	providerList = cloneProviderCatalog(published.Providers)
	if published.Registry == nil {
		providerRegistry = nil
	} else {
		providerRegistry = published.Registry.Clone()
	}
	providerPluginStatuses = cloneProviderStatuses(published.pluginStatuses)
	providerPresetReferences = maps.Clone(published.presetReferences)
	providerOwnerModes = maps.Clone(published.ownerModes)
	providerErr = nil
	return nil
}

// Providers performs the one-time composition of catalog metadata and runtime
// ownership. It securely loads trusted plugin bundles, compiles complete
// registrations, applies explicit rollout ownership, and commits catalog and
// registry together. Do not treat catalog presence as registration, discard a
// private plugin because it is absent from the public catalog, or keep a
// plugin's models while substituting another constructor.
func Providers(cfg *Config) ([]catalog.Provider, error) {
	if cfg != nil && cfg.providerScan != nil {
		return cloneProviderCatalog(cfg.providerScan.Providers), nil
	}
	providerOnce.Do(func() {
		scan, err := FreshProviderScan(context.Background(), cfg)
		if err == nil {
			publishProviderScan(scan, nil)
			return
		}
		providerStateMu.Lock()
		providerErr = err
		providerStateMu.Unlock()
	})
	providerStateMu.RLock()
	defer providerStateMu.RUnlock()
	return cloneProviderCatalog(providerList), providerErr
}

func FreshProviderScan(ctx context.Context, cfg *Config) (ProviderScan, error) {
	return freshProviderScan(ctx, cfg, env.New())
}

func freshProviderScan(ctx context.Context, cfg *Config, environment env.Env) (ProviderScan, error) {
	if cfg == nil {
		return ProviderScan{}, errors.New("provider scan requires configuration")
	}
	if environment == nil {
		environment = env.New()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, providerScanTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return ProviderScan{}, err
	}
	return scanProviders(ctx, cfg, environment)
}

func prepareConfiguredProviderOwner(providerID string, provider ProviderConfig) (ProviderConfig, error) {
	if provider.ID != "" && provider.ID != providerID {
		return provider, fmt.Errorf("provider %q declares conflicting ID %q", providerID, provider.ID)
	}
	if provider.Owner != nil {
		owner := *provider.Owner
		provider.Owner = &owner
	}
	if provider.Plugin != nil && provider.Preset != nil {
		return provider, fmt.Errorf("provider %q declares both plugin and preset ownership", providerID)
	}
	if provider.Owner == nil {
		switch {
		case provider.Plugin != nil:
			provider.Owner = &ProviderOwnerReference{Type: ProviderOwnerPlugin}
		case provider.Preset != nil:
			provider.Owner = &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat}
		default:
			if presetID, version, migrated := providerplugin.MigratedProviderPreset(providerID); migrated {
				provider.Owner = &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat}
				provider.Preset = &ProviderPresetReference{ID: presetID, Version: version}
			} else if construction, core := coreProviderConstruction(providerID); core {
				provider.Owner = &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: construction}
			} else {
				provider.Owner = &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}
			}
		}
	}
	if err := validateConfiguredProviderOwner(providerID, provider); err != nil {
		return provider, err
	}
	return provider, nil
}

func prepareConfiguredProviderOwners(cfg *Config) error {
	if cfg == nil || cfg.Providers == nil {
		return nil
	}
	for providerID, provider := range cfg.Providers.Seq2() {
		prepared, err := prepareConfiguredProviderOwner(providerID, provider)
		if err != nil {
			return err
		}
		cfg.Providers.Set(providerID, prepared)
	}
	return nil
}

func validateConfiguredProviderOwner(providerID string, provider ProviderConfig) error {
	if provider.Owner == nil {
		return fmt.Errorf("provider %q has no owner reference", providerID)
	}
	owner := provider.Owner
	if owner.Type != ProviderOwnerPlugin && owner.CompatibilityAdapter != "" {
		return fmt.Errorf("provider %q has a compatibility adapter without plugin ownership", providerID)
	}
	_, _, migrated := providerplugin.MigratedProviderPreset(providerID)
	coreConstruction, core := coreProviderConstruction(providerID)
	switch owner.Type {
	case ProviderOwnerPlugin:
		if provider.Plugin == nil || provider.Preset != nil {
			return fmt.Errorf("provider %q plugin ownership requires exactly one plugin reference", providerID)
		}
		if providerID == string(catalog.ProviderCopilot) || migrated {
			return fmt.Errorf("provider %q cannot use plugin ownership", providerID)
		}
	case ProviderOwnerPreset:
		if provider.Preset == nil || provider.Plugin != nil {
			return fmt.Errorf("provider %q preset ownership requires exactly one preset reference", providerID)
		}
		if core {
			return fmt.Errorf("provider %q cannot use preset ownership", providerID)
		}
		if owner.Construction == "" {
			owner.Construction = providerregistry.ConstructionOpenAICompat
		}
		if owner.Construction != providerregistry.ConstructionOpenAICompat {
			return fmt.Errorf("provider %q preset owner requires construction %q", providerID, providerregistry.ConstructionOpenAICompat)
		}
	case ProviderOwnerCore:
		if provider.Plugin != nil || provider.Preset != nil || !core {
			return fmt.Errorf("provider %q cannot use core ownership", providerID)
		}
		if owner.Construction == "" {
			owner.Construction = coreConstruction
		}
		if owner.Construction != coreConstruction {
			return fmt.Errorf("provider %q core owner requires construction %q", providerID, coreConstruction)
		}
	case ProviderOwnerCustom:
		if provider.Plugin != nil || provider.Preset != nil || core || migrated {
			return fmt.Errorf("provider %q cannot use custom ownership", providerID)
		}
		if owner.Construction == "" {
			owner.Construction = providerregistry.ConstructionOpenAICompat
		}
		if owner.Construction != providerregistry.ConstructionOpenAICompat {
			return fmt.Errorf("provider %q custom owner requires construction %q", providerID, providerregistry.ConstructionOpenAICompat)
		}
	default:
		return fmt.Errorf("provider %q has invalid owner type %q", providerID, owner.Type)
	}
	return nil
}

func scanProviders(ctx context.Context, cfg *Config, environment env.Env) (ProviderScan, error) {
	scan := ProviderScan{
		pluginStatuses:   make(map[string]providerplugin.Status),
		presetReferences: make(map[string]ProviderPresetReference),
		ownerModes:       make(map[string]providerregistry.OwnerMode),
	}
	if err := prepareConfiguredProviderOwners(cfg); err != nil {
		return scan, err
	}
	allIntegrated := providerregistry.Integrated()
	protectedProviderIDs := make(map[string]bool, len(allIntegrated))
	for _, registration := range allIntegrated {
		protectedProviderIDs[registration.ProviderID] = true
	}
	coreCatalog := []catalog.Provider{copilot.CatalogProvider()}
	integratedCatalog := []catalog.Provider{
		gemini.CatalogProvider(),
		codex.CatalogProvider(),
	}
	policy, err := parseProviderRolloutPolicyFromEnvironment(environment)
	if err != nil {
		return scan, err
	}

	if cfg.Options != nil && cfg.Options.DisableDefaultProviders {
		if err := rejectGenericReservedProviderClaims(cfg, protectedProviderIDs, nil); err != nil {
			return ProviderScan{}, err
		}
		registry, err := providerregistry.New()
		scan.Registry = registry
		return scan, err
	}

	var catalogErr error
	registrations := allIntegrated
	if policy.Profile == ProviderProfileCoreOnly || policy.Profile == ProviderProfilePluginNative {
		registrations = slices.DeleteFunc(registrations, func(registration providerregistry.Registration) bool {
			return registration.Construction != providerregistry.ConstructionCopilot
		})
		integratedCatalog = nil
	}
	activeCoreProviderIDs := make(map[string]bool, len(coreCatalog)+len(integratedCatalog))
	for _, provider := range append(cloneProviderCatalog(coreCatalog), integratedCatalog...) {
		activeCoreProviderIDs[string(provider.ID)] = true
	}
	if err := rejectGenericReservedProviderClaims(cfg, protectedProviderIDs, activeCoreProviderIDs); err != nil {
		return ProviderScan{}, err
	}

	pluginManager, pluginErr := providerplugin.NewManager(ctx, providerplugin.DefaultPaths(
		globalWorkspaceDirFromEnvironment(environment),
		globalCacheDirFromEnvironment(environment),
	))
	var pluginProviders []catalog.Provider
	var presetProviders []catalog.Provider
	var pluginRegistrations []providerregistry.Registration
	if pluginErr == nil {
		for _, status := range pluginManager.Snapshot().Plugins {
			if status.ID != "" {
				scan.pluginStatuses[status.ID] = status
			}
			for _, diagnostic := range status.Diagnostics {
				switch diagnostic.Code {
				case "migrated-preset-canonical-mismatch":
					expectedID, expectedVersion, _, _ := providerplugin.CanonicalMigratedProviderPreset(status.ProviderID)
					catalogErr = errors.Join(catalogErr, fmt.Errorf("provider preset claim %q must use canonical preset %s version %s with its canonical digest", status.ProviderID, expectedID, expectedVersion))
				}
			}
		}
		pluginProviders, pluginErr = pluginManager.CatalogProviders()
		presetProviders = pluginManager.CatalogPresets()

		acceptedPresetProviders := make(map[string]bool, len(presetProviders))
		for _, bundle := range pluginManager.RegisteredPresetBundles() {
			providerID := string(bundle.Manifest.Preset.ID)
			switch {
			case protectedProviderIDs[providerID]:
				catalogErr = errors.Join(catalogErr, fmt.Errorf("provider preset claim %q conflicts with a core provider", providerID))
			case migratedProviderPresetMismatch(providerID, bundle.Manifest.ID, bundle.Manifest.Version, bundle.Digest):
				expectedID, expectedVersion, _ := providerplugin.MigratedProviderPreset(providerID)
				catalogErr = errors.Join(catalogErr, fmt.Errorf("provider preset claim %q must use canonical preset %s version %s with its canonical digest", providerID, expectedID, expectedVersion))
			case configuredProviderOwnerExcludes(cfg, providerID, ProviderOwnerPreset):
				continue
			default:
				acceptedPresetProviders[providerID] = true
				scan.presetReferences[providerID] = ProviderPresetReference{ID: bundle.Manifest.ID, Version: bundle.Manifest.Version, Digest: bundle.Digest}
			}
		}
		presetProviders = slices.DeleteFunc(presetProviders, func(provider catalog.Provider) bool {
			return !acceptedPresetProviders[string(provider.ID)]
		})

		acceptedPluginProviders := make(map[string]bool, len(pluginProviders))
		if pluginErr == nil {
			for _, bundle := range pluginManager.RegisteredBundles() {
				providerID := bundle.Manifest.Provider.ID
				if isFullPluginReservedProviderID(providerID) {
					catalogErr = errors.Join(catalogErr, fmt.Errorf("provider plugin claim %q conflicts with a core provider", providerID))
					continue
				}
				if _, _, migrated := providerplugin.MigratedProviderPreset(providerID); migrated {
					catalogErr = errors.Join(catalogErr, fmt.Errorf("provider plugin claim %q conflicts with its reserved migrated preset", providerID))
					continue
				}
				if configuredProviderOwnerExcludes(cfg, providerID, ProviderOwnerPlugin) {
					continue
				}
				registration, err := providerregistry.FromManifest(bundle.Manifest, bundle.StaticText)
				if err != nil {
					pluginErr = errors.Join(pluginErr, err)
					continue
				}
				acceptedPluginProviders[providerID] = true
				pluginRegistrations = append(pluginRegistrations, registration)
			}
		}
		pluginProviders = slices.DeleteFunc(pluginProviders, func(provider catalog.Provider) bool {
			return !acceptedPluginProviders[string(provider.ID)]
		})
		pluginManager.Close()
	}
	if pluginErr != nil {
		catalogErr = errors.Join(catalogErr, fmt.Errorf("load provider plugins: %w", pluginErr))
	}

	scan.Providers = composeProviderPresets(coreCatalog, presetProviders)
	ownerModes := rolloutOwnerModes(policy, registrations, pluginRegistrations)
	scan.ownerModes = maps.Clone(ownerModes)
	var registryErr error
	scan.Providers, registryErr = composeProviderCatalog(scan.Providers, integratedCatalog, pluginProviders, ownerModes)
	if registryErr != nil {
		catalogErr = errors.Join(catalogErr, registryErr)
	}
	owned := make(map[string]bool, len(scan.Providers))
	for _, provider := range scan.Providers {
		owned[string(provider.ID)] = true
	}
	eligiblePlugins := make([]providerregistry.Registration, 0, len(pluginRegistrations))
	for _, registration := range pluginRegistrations {
		declared := slices.ContainsFunc(pluginProviders, func(provider catalog.Provider) bool {
			return string(provider.ID) == registration.ProviderID
		})
		if declared && owned[registration.ProviderID] {
			eligiblePlugins = append(eligiblePlugins, registration)
		}
	}
	registrations, registryErr = providerregistry.SelectOwners(registrations, eligiblePlugins, ownerModes)
	if registryErr != nil {
		catalogErr = errors.Join(catalogErr, fmt.Errorf("select provider owners: %w", registryErr))
	}
	scan.Registry, registryErr = providerregistry.New(registrations...)
	if registryErr != nil {
		catalogErr = errors.Join(catalogErr, fmt.Errorf("build provider capability registry: %w", registryErr))
	}
	return scan, catalogErr
}

func configuredProviderOwnerExcludes(cfg *Config, providerID string, expected ProviderOwnerType) bool {
	if cfg == nil || cfg.Providers == nil {
		return false
	}
	provider, configured := cfg.Providers.Get(providerID)
	return configured && provider.Owner != nil && provider.Owner.Type != expected
}

func migratedProviderPresetMismatch(providerID, presetID, version, digest string) bool {
	_, _, migrated := providerplugin.MigratedProviderPreset(providerID)
	return migrated && !providerplugin.IsCanonicalMigratedProviderPresetBundle(providerID, presetID, version, digest)
}

func isFullPluginReservedProviderID(providerID string) bool {
	return providerID == string(catalog.ProviderCopilot)
}

func validateForwardedProviderState(cfg *Config, providers map[string]ProviderConfig) error {
	for providerID, provider := range providers {
		if provider.ID != "" && provider.ID != providerID {
			return fmt.Errorf("forwarded provider %q declares conflicting ID %q", providerID, provider.ID)
		}
		if provider.Plugin != nil && provider.Preset != nil {
			return fmt.Errorf("forwarded provider %q declares both plugin and preset ownership", providerID)
		}
		if cfg != nil && cfg.Providers != nil {
			if persisted, found := cfg.Providers.Get(providerID); found && (persisted.Plugin != nil || persisted.Preset != nil) {
				return fmt.Errorf("forwarded provider %q conflicts with its persisted provider owner", providerID)
			}
		}
		if expectedID, expectedVersion, migrated := providerplugin.MigratedProviderPreset(providerID); migrated {
			if provider.Plugin != nil || provider.Preset == nil || !providerplugin.IsCanonicalMigratedProviderPreset(providerID, provider.Preset.ID, provider.Preset.Version) {
				return fmt.Errorf("forwarded provider %q must use canonical preset %s version %s", providerID, expectedID, expectedVersion)
			}
			if cfg != nil {
				if active, found := cfg.activeProviderPreset(providerID); found && !providerplugin.IsCanonicalMigratedProviderPresetBundle(providerID, active.ID, active.Version, active.Digest) {
					return fmt.Errorf("forwarded provider %q requires the canonical active preset digest", providerID)
				}
				if cfg.providerScan != nil && cfg.providerScan.Registry != nil {
					if _, found := cfg.providerScan.Registry.Lookup(providerID); found {
						return fmt.Errorf("forwarded provider %q conflicts with its reserved migrated preset", providerID)
					}
				}
			}
			continue
		}
		construction, core := coreProviderConstruction(providerID)
		if !core {
			continue
		}
		if provider.Preset != nil {
			return fmt.Errorf("forwarded provider %q cannot use preset ownership because its owner is selected by the active provider profile", providerID)
		}
		if providerID == string(catalog.ProviderCopilot) && provider.Plugin != nil {
			return fmt.Errorf("forwarded provider %q is reserved for its core catalog and registration", providerID)
		}
		if cfg == nil || cfg.providerScan == nil || cfg.providerScan.Registry == nil {
			return fmt.Errorf("forwarded provider %q requires its active core registration", providerID)
		}
		registration, found := cfg.providerScan.Registry.Lookup(providerID)
		if !found || registration.ProviderID != providerID {
			return fmt.Errorf("forwarded provider %q requires its active core registration", providerID)
		}
		if providerID == string(catalog.ProviderCopilot) {
			if registration.Manifest != nil || registration.Construction != construction {
				return fmt.Errorf("forwarded provider %q requires its active core registration", providerID)
			}
			continue
		}
		if registration.Manifest != nil {
			if !pluginReferenceMatches(provider.Plugin, registration) {
				return fmt.Errorf("forwarded provider %q must use active provider plugin %s version %s", providerID, registration.Manifest.ID, registration.Manifest.Version)
			}
			continue
		}
		if provider.Plugin != nil {
			return fmt.Errorf("forwarded provider %q is reserved for its active core catalog and registration", providerID)
		}
		if registration.Construction != construction {
			return fmt.Errorf("forwarded provider %q requires its active core registration", providerID)
		}
	}
	return nil
}

func coreProviderConstruction(providerID string) (providerregistry.Construction, bool) {
	switch providerID {
	case string(catalog.ProviderCopilot):
		return providerregistry.ConstructionCopilot, true
	case codex.ID:
		return providerregistry.ConstructionCodex, true
	case gemini.ID:
		return providerregistry.ConstructionGeminiAntigravity, true
	default:
		return "", false
	}
}

func rejectGenericReservedProviderClaims(cfg *Config, reserved, active map[string]bool) error {
	if cfg == nil || cfg.Providers == nil {
		return nil
	}
	for providerID := range reserved {
		if active[providerID] {
			continue
		}
		provider, configured := cfg.Providers.Get(providerID)
		if configured && provider.Plugin == nil && provider.Preset == nil {
			return fmt.Errorf("provider %q is reserved for its core catalog and registration, which are disabled by the active provider profile", providerID)
		}
	}
	return nil
}

// ProviderRegistry returns a clone of the capability registry atomically
// composed with the provider catalog. Providers must be called first; a nil
// result means initialization failed before ownership could be committed. Never
// synthesize a generic registration from catalog metadata when this is nil.
func ProviderRegistry() *providerregistry.Registry {
	providerStateMu.RLock()
	defer providerStateMu.RUnlock()
	if providerRegistry == nil {
		return nil
	}
	return providerRegistry.Clone()
}

// ProviderPluginAvailability reports scan and rollout state for diagnostics
// without changing provider ownership or selected configuration. Callers must
// not use a missing or inactive result as permission to replace that provider.
func ProviderPluginAvailability(pluginID string) (providerplugin.Status, providerregistry.OwnerMode, bool) {
	providerStateMu.RLock()
	defer providerStateMu.RUnlock()
	status, found := providerPluginStatuses[pluginID]
	mode := providerOwnerModes[status.ProviderID]
	return status.Clone(), mode, found
}

func ActiveProviderPreset(providerID string) (ProviderPresetReference, bool) {
	providerStateMu.RLock()
	defer providerStateMu.RUnlock()
	reference, found := providerPresetReferences[providerID]
	return reference, found
}

// ProviderCapabilities returns the current capability registry. Before catalog
// initialization, command and UI entrypoints may use bounded core-owned
// registrations; after initialization an intentionally empty registry remains
// empty. Do not bypass this boundary with direct integrated constructors for a
// plugin-owned provider.
func ProviderCapabilities() *providerregistry.Registry {
	if registry := ProviderRegistry(); registry != nil {
		return registry
	}
	policy, err := parseProviderRolloutPolicy()
	registrations := providerregistry.Integrated()
	if err != nil || policy.Profile != ProviderProfileIntegrated {
		registrations = slices.DeleteFunc(registrations, func(registration providerregistry.Registration) bool {
			return registration.Construction != providerregistry.ConstructionCopilot
		})
	}
	registry, _ := providerregistry.New(registrations...)
	return registry
}

// composeProviderCatalog creates one ordered provider registry projection.
// Base catalog entries keep their original slots. Integrated compatibility
// registrations append only when absent. Trusted plugin registrations append in
// manager order only when selected by the host rollout policy.
func parseProviderIDSet(value string) map[string]bool {
	result := map[string]bool{}
	for providerID := range strings.SplitSeq(value, ",") {
		if providerID = strings.TrimSpace(providerID); providerID != "" {
			result[providerID] = true
		}
	}
	return result
}

func EffectiveProviderRollout() (ProviderProfile, []string, error) {
	policy, err := parseProviderRolloutPolicy()
	if err != nil {
		return "", nil, err
	}
	enabled := make([]string, 0, len(policy.Enabled))
	for providerID := range policy.Enabled {
		enabled = append(enabled, providerID)
	}
	slices.Sort(enabled)
	return policy.Profile, enabled, nil
}

func parseProviderRolloutPolicy() (providerRolloutPolicy, error) {
	return parseProviderRolloutPolicyFromEnvironment(env.New())
}

func parseProviderRolloutPolicyFromEnvironment(environment env.Env) (providerRolloutPolicy, error) {
	if environment == nil {
		environment = env.New()
	}
	if strings.TrimSpace(environment.Get("CRUX_PROVIDER_PLUGIN_COMPAT")) != "" {
		return providerRolloutPolicy{}, fmt.Errorf("CRUX_PROVIDER_PLUGIN_COMPAT is unsupported; use CRUX_PROVIDER_PROFILE=plugin-compat with CRUX_PROVIDER_PLUGINS")
	}
	raw := strings.TrimSpace(environment.Get("CRUX_PROVIDER_PROFILE"))
	compiled := ProviderProfile(strings.TrimSpace(DefaultProviderProfile))
	if compiled == "" {
		compiled = ProviderProfilePluginCompat
	}
	policy := providerRolloutPolicy{
		Profile:         compiled,
		Enabled:         parseProviderIDSet(environment.Get("CRUX_PROVIDER_PLUGINS")),
		ExplicitProfile: raw != "",
	}
	if raw != "" {
		if compiled != ProviderProfilePluginCompat && ProviderProfile(raw) != compiled {
			return providerRolloutPolicy{}, fmt.Errorf("provider rollout profile %q exceeds compiled release profile %q", raw, compiled)
		}
		policy.Profile = ProviderProfile(raw)
	}
	switch policy.Profile {
	case ProviderProfileCoreOnly, ProviderProfileIntegrated, ProviderProfilePluginCompat, ProviderProfilePluginNative:
		return policy, nil
	default:
		return providerRolloutPolicy{}, fmt.Errorf("unknown provider rollout profile %q", raw)
	}
}

func rolloutOwnerModes(policy providerRolloutPolicy, integrated, plugins []providerregistry.Registration) map[string]providerregistry.OwnerMode {
	modes := make(map[string]providerregistry.OwnerMode, len(integrated)+len(plugins))
	integratedIDs := make(map[string]bool, len(integrated))
	for _, registration := range integrated {
		integratedIDs[registration.ProviderID] = true
		if policy.Profile == ProviderProfileIntegrated || registration.Construction == providerregistry.ConstructionCopilot {
			modes[registration.ProviderID] = providerregistry.OwnerIntegrated
		} else {
			modes[registration.ProviderID] = providerregistry.OwnerDisabled
		}
	}
	allowAll := len(policy.Enabled) == 0
	for _, plugin := range plugins {
		if isFullPluginReservedProviderID(plugin.ProviderID) {
			if integratedIDs[plugin.ProviderID] {
				modes[plugin.ProviderID] = providerregistry.OwnerIntegrated
			} else {
				modes[plugin.ProviderID] = providerregistry.OwnerDisabled
			}
			continue
		}
		allowed := allowAll || policy.Enabled[plugin.ProviderID]
		if policy.Profile == ProviderProfileCoreOnly {
			modes[plugin.ProviderID] = providerregistry.OwnerDisabled
			continue
		}
		if !allowed || policy.Profile == ProviderProfileIntegrated {
			if !integratedIDs[plugin.ProviderID] {
				modes[plugin.ProviderID] = providerregistry.OwnerDisabled
			}
			continue
		}
		switch policy.Profile {
		case ProviderProfilePluginNative:
			if plugin.CompatibilityAdapter == "" {
				modes[plugin.ProviderID] = providerregistry.OwnerPluginNative
			} else {
				modes[plugin.ProviderID] = providerregistry.OwnerDisabled
			}
		case ProviderProfilePluginCompat:
			if plugin.CompatibilityAdapter == "" {
				modes[plugin.ProviderID] = providerregistry.OwnerPluginNative
			} else {
				modes[plugin.ProviderID] = providerregistry.OwnerPluginCompat
			}
		}
	}
	return modes
}

func composeProviderPresets(base, presets []catalog.Provider) []catalog.Provider {
	result := cloneProviderCatalog(base)
	positions := make(map[catalog.ProviderID]int, len(result)+len(presets))
	for i, provider := range result {
		positions[provider.ID] = i
	}
	for _, preset := range presets {
		if _, exists := positions[preset.ID]; exists {
			continue
		}
		positions[preset.ID] = len(result)
		result = append(result, cloneProvider(preset))
	}
	return result
}

func composeProviderCatalog(base, integrated, plugins []catalog.Provider, modes map[string]providerregistry.OwnerMode) ([]catalog.Provider, error) {
	result := slices.DeleteFunc(cloneProviderCatalog(base), func(provider catalog.Provider) bool {
		return modes[string(provider.ID)] == providerregistry.OwnerDisabled
	})
	positions := make(map[catalog.ProviderID]int, len(base)+len(integrated)+len(plugins))
	for i, provider := range result {
		positions[provider.ID] = i
	}
	for _, provider := range integrated {
		if modes[string(provider.ID)] == providerregistry.OwnerDisabled {
			continue
		}
		if _, exists := positions[provider.ID]; exists {
			continue
		}
		positions[provider.ID] = len(result)
		result = append(result, cloneProvider(provider))
	}
	var conflicts []error
	for _, provider := range plugins {
		if isFullPluginReservedProviderID(string(provider.ID)) {
			conflicts = append(conflicts, fmt.Errorf("provider plugin claim %q conflicts with a core provider", provider.ID))
			continue
		}
		mode := modes[string(provider.ID)]
		if mode == providerregistry.OwnerDisabled || mode == providerregistry.OwnerIntegrated {
			continue
		}
		if position, exists := positions[provider.ID]; exists {
			if mode == providerregistry.OwnerPluginCompat || mode == providerregistry.OwnerPluginNative {
				result[position] = cloneProvider(provider)
				continue
			}
			conflicts = append(conflicts, fmt.Errorf("provider plugin claim %q conflicts with an existing catalog owner", provider.ID))
			continue
		}
		positions[provider.ID] = len(result)
		result = append(result, cloneProvider(provider))
	}
	return result, errors.Join(conflicts...)
}

func cloneProviderCatalog(providers []catalog.Provider) []catalog.Provider {
	result := make([]catalog.Provider, len(providers))
	for i, provider := range providers {
		result[i] = cloneProvider(provider)
	}
	return result
}

func cloneProvider(provider catalog.Provider) catalog.Provider {
	provider.Models = slices.Clone(provider.Models)
	for i := range provider.Models {
		provider.Models[i].ReasoningLevels = slices.Clone(provider.Models[i].ReasoningLevels)
		provider.Models[i].Options.Temperature = clonePointer(provider.Models[i].Options.Temperature)
		provider.Models[i].Options.TopP = clonePointer(provider.Models[i].Options.TopP)
		provider.Models[i].Options.TopK = clonePointer(provider.Models[i].Options.TopK)
		provider.Models[i].Options.FrequencyPenalty = clonePointer(provider.Models[i].Options.FrequencyPenalty)
		provider.Models[i].Options.PresencePenalty = clonePointer(provider.Models[i].Options.PresencePenalty)
		provider.Models[i].Options.ProviderOptions = cloneProviderOptions(provider.Models[i].Options.ProviderOptions)
	}
	provider.DefaultHeaders = maps.Clone(provider.DefaultHeaders)
	return provider
}
