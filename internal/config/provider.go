package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"github.com/charmbracelet/x/etag"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/home"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
)

type syncer[T any] interface {
	Get(context.Context) (T, error)
}

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
	LegacyCompat    map[string]bool
	ExplicitProfile bool
}

var (
	providerOnce           sync.Once
	providerList           []catwalk.Provider
	providerRegistry       *providerregistry.Registry
	providerPluginStatuses map[string]providerplugin.Status
	providerOwnerModes     map[string]providerregistry.OwnerMode
	providerErr            error
)

// file to cache provider data
func cachePathFor(name string) string {
	xdgDataHome := os.Getenv("XDG_DATA_HOME")
	if xdgDataHome != "" {
		return filepath.Join(xdgDataHome, appName, name+".json")
	}

	// return the path to the main data directory
	// for Windows, it should be in `%LOCALAPPDATA%/crux/`
	// for Linux and macOS, it should be in `$HOME/.ai-cli/data/crux/`
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			localAppData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
		return filepath.Join(localAppData, appName, name+".json")
	}

	return filepath.Join(home.Data(), appName, name+".json")
}

// UpdateProviders updates the Catwalk providers list from a specified source.
func UpdateProviders(pathOrURL string) error {
	var providers []catwalk.Provider
	if pathOrURL == "" {
		pathOrURL = "embedded"
	}

	switch {
	case pathOrURL == "embedded":
		providers = embedded.GetAll()
	case strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://"):
		var err error
		providers, err = catwalk.NewWithURL(pathOrURL).GetProviders(context.Background(), "")
		if err != nil {
			return fmt.Errorf("failed to fetch providers: %w", err)
		}
	default:
		content, err := os.ReadFile(pathOrURL)
		if err != nil {
			return fmt.Errorf("failed to read file: %w", err)
		}
		if err := json.Unmarshal(content, &providers); err != nil {
			return fmt.Errorf("failed to unmarshal provider data: %w", err)
		}
		if len(providers) == 0 {
			return fmt.Errorf("no providers found in the provided source")
		}
	}

	providers = retainedCatalogProviders(providers)
	if len(providers) == 0 {
		return fmt.Errorf("no supported OpenAI-compatible providers found in the provided source")
	}
	if err := newCache[[]catwalk.Provider](cachePathFor("providers")).Store(providers); err != nil {
		return fmt.Errorf("failed to save providers to cache: %w", err)
	}

	slog.Info("Providers updated successfully", "count", len(providers), "from", pathOrURL, "to", cachePathFor("providers"))
	return nil
}

var catwalkSyncer = &catwalkSync{}

func Providers(cfg *Config) ([]catwalk.Provider, error) {
	providerOnce.Do(func() {
		var wg sync.WaitGroup
		providers := csync.NewSlice[catwalk.Provider]()
		customProvidersOnly := cfg.Options.DisableDefaultProviders

		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		var catalogErr error

		wg.Go(func() {
			if customProvidersOnly {
				return
			}
			catwalkSyncer.Init(cachePathFor("providers"))
			items, err := catwalkSyncer.Get(ctx)
			if err != nil {
				catalogErr = err
			}
			providers.Append(items...)
		})

		wg.Wait()

		providerList = slices.Collect(providers.Seq())
		var registrations []providerregistry.Registration
		if !customProvidersOnly {
			policy, policyErr := parseProviderRolloutPolicy()
			if policyErr != nil {
				catalogErr = errors.Join(catalogErr, policyErr)
				policy = providerRolloutPolicy{Profile: ProviderProfileCoreOnly}
			}
			registrations = providerregistry.Integrated()
			integrated := []catwalk.Provider{
				gemini.CatwalkProvider(),
				codex.CatwalkProvider(),
			}
			if policy.Profile == ProviderProfileCoreOnly || policy.Profile == ProviderProfilePluginNative {
				providerList = filterExcludedIntegratedCatalog(providerList, registrations)
				registrations = slices.DeleteFunc(registrations, func(registration providerregistry.Registration) bool {
					return registration.Construction != providerregistry.ConstructionCopilot
				})
				integrated = nil
			}
			pluginManager, pluginErr := providerplugin.NewManager(ctx, providerplugin.DefaultPaths(GlobalWorkspaceDir(), GlobalCacheDir()))
			var pluginProviders []catwalk.Provider
			var presetProviders []catwalk.Provider
			var pluginRegistrations []providerregistry.Registration
			if pluginErr == nil {
				providerPluginStatuses = make(map[string]providerplugin.Status)
				for _, status := range pluginManager.Snapshot().Plugins {
					if status.ID != "" {
						providerPluginStatuses[status.ID] = status
					}
				}
				pluginProviders, pluginErr = pluginManager.CatalogProviders()
				presetProviders = pluginManager.CatalogPresets()
				if pluginErr == nil {
					for _, bundle := range pluginManager.RegisteredBundles() {
						registration, err := providerregistry.FromManifest(bundle.Manifest, bundle.StaticText)
						if err != nil {
							pluginErr = errors.Join(pluginErr, err)
							continue
						}
						pluginRegistrations = append(pluginRegistrations, registration)
					}
				}
				pluginManager.Close()
			}
			if pluginErr != nil {
				catalogErr = errors.Join(catalogErr, fmt.Errorf("load provider plugins: %w", pluginErr))
			}
			providerList = composeProviderPresets(providerList, presetProviders)
			ownerModes := rolloutOwnerModes(policy, registrations, pluginRegistrations)
			providerOwnerModes = maps.Clone(ownerModes)
			coreOwned := make(map[string]bool, len(providerList)+len(integrated))
			for _, provider := range append(cloneProviderCatalog(providerList), integrated...) {
				coreOwned[string(provider.ID)] = true
			}
			var registryErr error
			providerList, registryErr = composeProviderCatalog(providerList, integrated, pluginProviders, ownerModes)
			if registryErr != nil {
				catalogErr = errors.Join(catalogErr, registryErr)
			}
			owned := make(map[string]bool, len(providerList))
			for _, provider := range providerList {
				owned[string(provider.ID)] = true
			}
			eligiblePlugins := make([]providerregistry.Registration, 0, len(pluginRegistrations))
			for _, registration := range pluginRegistrations {
				declared := slices.ContainsFunc(pluginProviders, func(provider catwalk.Provider) bool {
					return string(provider.ID) == registration.ProviderID
				})
				pluginSelected := ownerModes[registration.ProviderID] == providerregistry.OwnerPluginCompat || ownerModes[registration.ProviderID] == providerregistry.OwnerPluginNative
				if declared && owned[registration.ProviderID] && (!coreOwned[registration.ProviderID] || pluginSelected) {
					eligiblePlugins = append(eligiblePlugins, registration)
				}
			}
			registrations, registryErr = providerregistry.SelectOwners(registrations, eligiblePlugins, ownerModes)
			if registryErr != nil {
				catalogErr = errors.Join(catalogErr, fmt.Errorf("select provider owners: %w", registryErr))
			}
		}
		providerRegistry, providerErr = providerregistry.New(registrations...)
		if providerErr != nil {
			catalogErr = errors.Join(catalogErr, fmt.Errorf("build provider capability registry: %w", providerErr))
		}
		providerErr = catalogErr
	})
	return cloneProviderCatalog(providerList), providerErr
}

// ProviderRegistry returns the immutable capability registry composed with the
// provider catalog. Providers must be called first; a nil result means catalog
// initialization failed before a registry could be committed.
func ProviderRegistry() *providerregistry.Registry {
	if providerRegistry == nil {
		return nil
	}
	return providerRegistry.Clone()
}

// ProviderCapabilities returns the current capability registry. Before catalog
// initialization, command and UI entrypoints use the core-owned registrations;
// after initialization an intentionally empty registry remains empty.
func ProviderPluginAvailability(pluginID string) (providerplugin.Status, providerregistry.OwnerMode, bool) {
	status, found := providerPluginStatuses[pluginID]
	mode := providerOwnerModes[status.ProviderID]
	return status, mode, found
}

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

func ProviderBehaviorCapabilities(providerID string) (providerregistry.Registration, bool) {
	if registration, ok := ProviderCapabilities().Lookup(providerID); ok && registration.ProviderID == providerID {
		return registration, true
	}
	for _, registration := range providerregistry.Integrated() {
		if registration.ProviderID == providerID {
			return registration, true
		}
	}
	return providerregistry.Registration{}, false
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
	raw := strings.TrimSpace(os.Getenv("CRUX_PROVIDER_PROFILE"))
	compiled := ProviderProfile(strings.TrimSpace(DefaultProviderProfile))
	if compiled == "" {
		compiled = ProviderProfilePluginCompat
	}
	policy := providerRolloutPolicy{
		Profile:         compiled,
		Enabled:         parseProviderIDSet(os.Getenv("CRUX_PROVIDER_PLUGINS")),
		LegacyCompat:    parseProviderIDSet(os.Getenv("CRUX_PROVIDER_PLUGIN_COMPAT")),
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

func filterExcludedIntegratedCatalog(catalog []catwalk.Provider, registrations []providerregistry.Registration) []catwalk.Provider {
	excluded := make(map[string]bool, len(registrations))
	for _, registration := range registrations {
		if registration.Construction != providerregistry.ConstructionCopilot {
			excluded[registration.ProviderID] = true
		}
	}
	result := cloneProviderCatalog(catalog)
	return slices.DeleteFunc(result, func(provider catwalk.Provider) bool {
		return excluded[string(provider.ID)]
	})
}

func composeProviderPresets(base, presets []catwalk.Provider) []catwalk.Provider {
	result := cloneProviderCatalog(base)
	positions := make(map[catwalk.InferenceProvider]int, len(result)+len(presets))
	for i, provider := range result {
		positions[provider.ID] = i
	}
	for _, preset := range presets {
		if position, exists := positions[preset.ID]; exists {
			result[position] = cloneProvider(preset)
			continue
		}
		positions[preset.ID] = len(result)
		result = append(result, cloneProvider(preset))
	}
	return result
}

func composeProviderCatalog(base, integrated, plugins []catwalk.Provider, modes map[string]providerregistry.OwnerMode) ([]catwalk.Provider, error) {
	result := slices.DeleteFunc(cloneProviderCatalog(base), func(provider catwalk.Provider) bool {
		return modes[string(provider.ID)] == providerregistry.OwnerDisabled
	})
	positions := make(map[catwalk.InferenceProvider]int, len(base)+len(integrated)+len(plugins))
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

func cloneProviderCatalog(providers []catwalk.Provider) []catwalk.Provider {
	result := make([]catwalk.Provider, len(providers))
	for i, provider := range providers {
		result[i] = cloneProvider(provider)
	}
	return result
}

func cloneProvider(provider catwalk.Provider) catwalk.Provider {
	provider.Models = slices.Clone(provider.Models)
	for i := range provider.Models {
		provider.Models[i].ReasoningLevels = slices.Clone(provider.Models[i].ReasoningLevels)
		provider.Models[i].Options.ProviderOptions = maps.Clone(provider.Models[i].Options.ProviderOptions)
	}
	provider.DefaultHeaders = maps.Clone(provider.DefaultHeaders)
	return provider
}

type cache[T any] struct {
	path string
}

func newCache[T any](path string) cache[T] {
	return cache[T]{path: path}
}

func (c cache[T]) Get() (T, string, error) {
	var v T
	data, err := os.ReadFile(c.path)
	if err != nil {
		return v, "", fmt.Errorf("failed to read provider cache file: %w", err)
	}

	if err := json.Unmarshal(data, &v); err != nil {
		return v, "", fmt.Errorf("failed to unmarshal provider data from cache: %w", err)
	}

	return v, etag.Of(data), nil
}

func (c cache[T]) Store(v T) error {
	slog.Info("Saving provider data to disk", "path", c.path)
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("failed to create directory for provider cache: %w", err)
	}

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal provider data: %w", err)
	}

	// Written through a temporary file and renamed into place. Several Crux
	// instances start independently and race to refresh this cache, and a
	// truncating write would let one of them read a half-written catalog and
	// silently fall back to the bundled copy.
	if err := atomicWriteFile(c.path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write provider data to cache: %w", err)
	}
	return nil
}
