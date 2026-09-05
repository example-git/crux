package config

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	powernapConfig "github.com/charmbracelet/x/powernap/pkg/config"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/discover"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/filepathext"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/home"
	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/shellconfig"
	"github.com/qjebbs/go-jsons"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Load loads configuration and installs the resolved selections used by the
// runtime. Persisted plugin-backed selections remain authoritative even when
// their integration is temporarily unavailable. Do not make startup success
// depend on rewriting them to an unrelated available provider; retain them and
// let construction report the unavailable selected integration clearly.
func Load(workingDir, dataDir string, debug bool) (*ConfigStore, error) {
	return loadWithEnvironment(workingDir, dataDir, debug, snapshotEnvironment(), true)
}

func LoadIsolated(workingDir, dataDir string, debug bool, baseEnvironment env.Env) (*ConfigStore, error) {
	if baseEnvironment == nil {
		return nil, errors.New("isolated config load requires a base environment")
	}
	return loadWithEnvironment(workingDir, dataDir, debug, cloneEnvironment(baseEnvironment), false)
}

func SnapshotEnvironment() env.Env {
	return snapshotEnvironment()
}

func loadWithEnvironment(workingDir, dataDir string, debug bool, baseEnvironment env.Env, publishProcessState bool) (*ConfigStore, error) {
	globalConfigPath := globalConfigFromEnvironment(baseEnvironment)
	globalDataPath := globalConfigDataFromEnvironment(appName, baseEnvironment)
	notificationMigration := prepareDisableNotificationsMigration(globalConfigPath, globalDataPath)
	preimages, err := captureConfigPreimages(globalConfigPath, globalDataPath)
	if err != nil {
		return nil, fmt.Errorf("capture startup config state: %w", err)
	}
	configPaths := lookupConfigsFromEnvironment(workingDir, baseEnvironment)

	cfg, loadedPaths, err := loadFromConfigPathsWithOverrides(context.Background(), configPaths, notificationMigration.overrides, baseEnvironment)
	if err != nil {
		return nil, fmt.Errorf("failed to load config from paths %v: %w", configPaths, err)
	}

	if err := cfg.setDefaultsFromEnvironment(workingDir, dataDir, baseEnvironment); err != nil {
		return nil, fmt.Errorf("apply configuration defaults: %w", err)
	}

	store := &ConfigStore{
		config:              cfg,
		workingDir:          workingDir,
		baseEnvironment:     baseEnvironment,
		publishProcessState: publishProcessState,
		globalDataPath:      globalDataPath,
		workspacePath:       filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName)),
		loadedPaths:         loadedPaths,
	}

	if debug {
		cfg.Options.Debug = true
	}

	// Load workspace config last so it has highest priority.
	if wsData, err := os.ReadFile(store.workspacePath); err == nil && len(wsData) > 0 {
		if !json.Valid(wsData) {
			return nil, fmt.Errorf("invalid JSON in config file %s", store.workspacePath)
		}
		merged, mergeErr := loadFromBytes(append([][]byte{mustMarshalConfig(cfg)}, wsData))
		if mergeErr == nil {
			// Preserve defaults that setDefaults already applied.
			dataDir := cfg.Options.DataDirectory
			*cfg = *merged
			if err := cfg.setDefaultsFromEnvironment(workingDir, dataDir, baseEnvironment); err != nil {
				return nil, fmt.Errorf("apply workspace configuration defaults: %w", err)
			}
			store.config = cfg
			store.loadedPaths = append(store.loadedPaths, store.workspacePath)
		}
	}
	cfg.captureExplicitModels()

	// Validate hooks after all config merging is complete so workspace
	// hooks also get their matcher regexes compiled.
	if err := cfg.ValidateHooks(); err != nil {
		return nil, fmt.Errorf("invalid hook configuration: %w", err)
	}
	if err := cfg.Options.validatePromptOptions(); err != nil {
		return nil, fmt.Errorf("invalid prompt options: %w", err)
	}

	if !isInsideWorktree() {
		const depth = 2
		const items = 100
		slog.Warn("No git repository detected in working directory, will limit file walk operations", "depth", depth, "items", items)
		assignIfNil(&cfg.Tools.Ls.MaxDepth, depth)
		assignIfNil(&cfg.Tools.Ls.MaxItems, items)
		assignIfNil(&cfg.Options.TUI.Completions.MaxDepth, depth)
		assignIfNil(&cfg.Options.TUI.Completions.MaxItems, items)
	}

	if isAppleTerminalFromEnvironment(baseEnvironment) {
		slog.Warn("Detected Apple Terminal, enabling transparent mode")
		assignIfNil(&cfg.Options.TUI.Transparent, true)
	}

	candidateEnv, valueResolver, resolvedEnv, err := cfg.buildEnvironmentFrom(baseEnvironment)
	if err != nil {
		return nil, fmt.Errorf("build candidate environment: %w", err)
	}
	scan, err := freshProviderScan(context.Background(), cfg, baseEnvironment)
	if err != nil {
		return nil, fmt.Errorf("failed to scan providers: %w", err)
	}
	cfg.bindProviderScan(scan)
	store.knownProviders = cloneProviderCatalog(scan.Providers)
	if scan.Registry != nil {
		store.providerRegistry = scan.Registry.Clone()
	}
	store.resolver = valueResolver

	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	var pendingOwners map[string]ProviderOwnerReference
	var pendingPlugins map[string]ProviderPluginReference
	var pendingPresets map[string]ProviderPresetReference
	collectMigration := func(owners map[string]ProviderOwnerReference, plugins map[string]ProviderPluginReference, presets map[string]ProviderPresetReference) error {
		pendingOwners = maps.Clone(owners)
		pendingPlugins = maps.Clone(plugins)
		pendingPresets = maps.Clone(presets)
		return nil
	}
	if err := cfg.configureProvidersWithMigration(context.Background(), store, candidateEnv, valueResolver, store.knownProviders, collectMigration); err != nil {
		return nil, fmt.Errorf("failed to configure providers: %w", err)
	}

	pendingModelFields := make(map[string]any)
	if !cfg.IsConfigured() {
		slog.Warn("No providers configured")
	} else {
		resolved, resolveErr := resolveSelectedModels(cfg, store.knownProviders)
		if resolveErr != nil {
			return nil, fmt.Errorf("failed to configure selected models: %w", resolveErr)
		}
		cfg.Models[SelectedModelTypeLarge] = resolved.Large
		cfg.Models[SelectedModelTypeSmall] = resolved.Small

		if resolved.LargeFallback {
			maps.Copy(pendingModelFields, store.updatePreferredModelFields(cfg, SelectedModelTypeLarge, resolved.Large))
		}
		if resolved.SmallFallback {
			maps.Copy(pendingModelFields, store.updatePreferredModelFields(cfg, SelectedModelTypeSmall, resolved.Small))
		}
	}
	cfg.SetupAgents()
	if err := commitStartupCorrections(store, notificationMigration, pendingModelFields, preimages); err != nil {
		rollbackErr := restoreConfigPreimages(preimages)
		return nil, errors.Join(fmt.Errorf("commit startup config corrections: %w", err), rollbackErr)
	}
	if err := captureConfigPostimages(preimages); err != nil {
		rollbackErr := restoreConfigPreimages(preimages)
		return nil, errors.Join(fmt.Errorf("capture startup config correction postimages: %w", err), rollbackErr)
	}
	migrationExpectedData, migrationExpectedExists, err := configPostimage(preimages, store.globalDataPath)
	if err != nil {
		rollbackErr := restoreConfigPreimages(preimages)
		return nil, errors.Join(fmt.Errorf("resolve startup provider ownership migration postimage: %w", err), rollbackErr)
	}
	_, migrationBeforeHash, migrationBeforeExists, err := fileBytesAndHash(store.globalDataPath)
	if err != nil {
		rollbackErr := restoreConfigPreimages(preimages)
		return nil, errors.Join(fmt.Errorf("capture provider ownership migration preimage: %w", err), rollbackErr)
	}
	if err := store.migrateProviderReferencesIfCurrent(pendingOwners, pendingPlugins, pendingPresets, migrationExpectedData, migrationExpectedExists); err != nil {
		rollbackErr := restoreConfigPreimages(preimages)
		return nil, errors.Join(fmt.Errorf("migrate provider ownership: %w", err), rollbackErr)
	}
	_, migrationAfterHash, migrationAfterExists, migrationInspectErr := fileBytesAndHash(store.globalDataPath)
	ownershipChanged := migrationBeforeExists != migrationAfterExists || migrationBeforeHash != migrationAfterHash
	_, startupInspectErr := inspectStartupConfigPreimageChanged(preimages, store.globalDataPath)
	if err := errors.Join(migrationInspectErr, startupInspectErr); err != nil {
		var migrationRollbackErr error
		if ownershipChanged {
			migrationRollbackErr = store.rollbackProviderMigration()
		}
		rollbackErr := restoreConfigPreimages(preimages)
		return nil, errors.Join(fmt.Errorf("inspect provider ownership migration: %w", err), migrationRollbackErr, rollbackErr)
	}
	publish := func(published ProviderScan) error {
		if publishProcessState {
			if err := applyEnvironment(baseEnvironment, nil, resolvedEnv); err != nil {
				return err
			}
			store.appliedEnvironment = maps.Clone(resolvedEnv)
		}
		store.effectiveEnvironment = cloneEnvironment(candidateEnv)
		registerConfigSecrets(cfg)
		store.knownProviders = cloneProviderCatalog(published.Providers)
		if published.Registry == nil {
			store.providerRegistry = nil
		} else {
			store.providerRegistry = published.Registry.Clone()
		}
		store.config = cfg
		return nil
	}
	var publishErr error
	if publishProcessState {
		publishErr = publishConfiguredProviderScan(scan, publish)
	} else {
		publishErr = publish(cloneProviderScan(scan))
	}
	if publishErr != nil {
		var migrationRollbackErr error
		if ownershipChanged {
			migrationRollbackErr = store.rollbackProviderMigration()
		}
		rollbackErr := restoreConfigPreimages(preimages)
		return nil, errors.Join(fmt.Errorf("publish startup generation: %w", publishErr), migrationRollbackErr, rollbackErr)
	}

	store.captureStalenessSnapshot(append(slices.Clone(configPaths), loadedPaths...))

	return store, nil
}

// mustMarshalConfig marshals the config to JSON bytes, returning empty JSON on
// error.
func mustMarshalConfig(cfg *Config) []byte {
	data, err := json.Marshal(cfg)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func (c *Config) completeProviderOwner(providerID string, provider ProviderConfig) ProviderConfig {
	if provider.Owner == nil {
		return provider
	}
	owner := *provider.Owner
	provider.Owner = &owner
	switch owner.Type {
	case ProviderOwnerPlugin:
		if provider.Plugin == nil {
			return provider
		}
		registration, found := c.providerCapabilities().Lookup(providerID)
		if !found || registration.Manifest == nil || registration.Manifest.ID != provider.Plugin.ID {
			return provider
		}
		plugin := *provider.Plugin
		if plugin.Version == "" {
			plugin.Version = registration.Manifest.Version
		}
		if plugin.Version != registration.Manifest.Version {
			return provider
		}
		provider.Plugin = &plugin
		if owner.Construction == "" {
			owner.Construction = registration.Construction
		}
		if owner.CompatibilityAdapter == "" {
			owner.CompatibilityAdapter = registration.CompatibilityAdapter
		}
	case ProviderOwnerPreset:
		if provider.Preset == nil {
			return provider
		}
		active, found := c.activeProviderPreset(providerID)
		if !found || active.ID != provider.Preset.ID {
			return provider
		}
		preset := *provider.Preset
		if preset.Version == "" {
			preset.Version = active.Version
		}
		if preset.Version != active.Version {
			return provider
		}
		if preset.Digest == "" {
			preset.Digest = active.Digest
		}
		provider.Preset = &preset
	}
	return provider
}

// configureProviders is the durable provider ownership boundary. Plugin and
// preset markers, credentials, catalogs, and custom configuration must survive
// a temporary registration absence so that a later startup can reconstruct the
// same provider. Do not reinterpret an unavailable owned provider as a generic
// endpoint, run generic model discovery for it, or delete it because the
// current process cannot construct it.
func (c *Config) configureProviders(ctx context.Context, store *ConfigStore, env env.Env, resolver VariableResolver, knownProviders []catalog.Provider) error {
	return c.configureProvidersWithMigration(ctx, store, env, resolver, knownProviders, store.migrateProviderReferences)
}

func (c *Config) configureProvidersWithMigration(ctx context.Context, store *ConfigStore, env env.Env, resolver VariableResolver, knownProviders []catalog.Provider, migrate func(map[string]ProviderOwnerReference, map[string]ProviderPluginReference, map[string]ProviderPresetReference) error) error {
	if err := prepareConfiguredProviderOwners(c); err != nil {
		return err
	}
	knownProviderNames := make(map[string]bool)

	// When disable_default_providers is enabled, skip all core and installed
	// provider catalogs. Users must fully specify any providers they want.
	// We skip to the custom provider validation loop which handles all
	// user-configured providers uniformly.
	if c.Options.DisableDefaultProviders {
		knownProviders = nil
	}

	for id, provider := range c.Providers.Seq2() {
		c.Providers.Set(id, c.completeProviderOwner(id, provider))
	}

	for _, p := range knownProviders {
		knownProviderNames[string(p.ID)] = true
		config, configExists := c.Providers.Get(string(p.ID))
		if configExists {
			if c.isUnavailableRegisteredProvider(string(p.ID)) {
				continue
			}
			if err := c.ValidateProviderConfiguration(string(p.ID), config.Configuration); err != nil {
				return err
			}
		}
		// if the user configured a known provider we need to allow it to override a couple of parameters
		if configExists {
			if config.BaseURL != "" {
				p.APIEndpoint = config.BaseURL
			}
			if config.APIKey != "" {
				p.APIKey = config.APIKey
			}
			if len(config.Models) > 0 {
				models := []catalog.Model{}
				seen := make(map[string]bool)

				for _, model := range config.Models {
					if seen[model.ID] {
						continue
					}
					seen[model.ID] = true
					if model.Name == "" {
						model.Name = model.ID
					}
					models = append(models, model)
				}
				for _, model := range p.Models {
					if seen[model.ID] {
						continue
					}
					seen[model.ID] = true
					if model.Name == "" {
						model.Name = model.ID
					}
					models = append(models, model)
				}

				p.Models = models
			}
		}

		headers := map[string]string{}
		if len(p.DefaultHeaders) > 0 {
			maps.Copy(headers, p.DefaultHeaders)
		}
		if len(config.ExtraHeaders) > 0 {
			maps.Copy(headers, config.ExtraHeaders)
		}
		// Provider headers use the same error contract as MCP headers:
		// a failing $(...) aborts the provider load with a clear
		// message, and a header that resolves to the empty string
		// (unset bare $VAR under lenient nounset, $(echo), or literal
		// "") is dropped from the outgoing request.
		for k, v := range headers {
			resolved, err := resolver.ResolveValue(v)
			if err != nil {
				return fmt.Errorf("resolving provider %s header %q: %w", p.ID, k, err)
			}
			if resolved == "" {
				delete(headers, k)
				continue
			}
			headers[k] = resolved
		}
		// Start from user config so all user fields survive without
		// explicit copying. Overlay catalog identity/endpoint fields
		// (already merged with user overrides above).
		prepared := config
		prepared.ID = string(p.ID)
		prepared.Name = p.Name
		prepared.BaseURL = p.APIEndpoint
		prepared.APIKey = p.APIKey
		prepared.APIKeyTemplate = p.APIKey // Store original template for re-resolution
		prepared.Type = p.Type
		prepared.Models = p.Models
		prepared.ExtraHeaders = headers
		if prepared.Owner == nil {
			if registration, ok := c.providerCapabilities().Lookup(string(p.ID)); ok {
				prepared.Owner = providerOwnerReferenceForRegistration(registration)
				if registration.Manifest != nil {
					prepared.Plugin = &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}
				}
			} else if reference, ok := c.activeProviderPreset(string(p.ID)); ok {
				prepared.Owner = providerPresetOwnerReference()
				prepared.Preset = &reference
			}
		}
		if prepared.ExtraParams == nil {
			prepared.ExtraParams = make(map[string]string)
		}

		if config.OAuthToken != nil {
			registration, ok := c.providerRegistration(c.providerCapabilities(), string(p.ID))
			if ok && registration.Construction == providerregistry.ConstructionCopilot {
				prepared.SetupGitHubCopilot()
			}
		}

		// When a provider is explicitly disabled, skip credential
		// validation entirely and preserve it in the map so the disable
		// flag survives reloads and can be re-enabled from the UI.
		if config.Disable {
			c.Providers.Set(string(p.ID), prepared)
			continue
		}

		anonymous := false
		if registration, ok := c.ProviderRegistration(string(p.ID)); ok && registration.Manifest != nil && registration.OAuth == nil {
			anonymous = true
			for _, credential := range registration.Manifest.Capabilities.Credentials {
				if credential.Kind != "none" {
					anonymous = false
					break
				}
			}
		}
		if anonymous && !configExists {
			if err := c.ValidateProviderConfiguration(string(p.ID), prepared.Configuration); err != nil {
				return err
			}
		}

		// If the provider API key is missing, skip it. Copilot OAuth setup
		// above replaces the catalog template before this check.
		v, err := resolver.ResolveValue(p.APIKey)
		if v == "" && !anonymous || err != nil {
			if configExists {
				slog.Warn("Skipping provider due to missing API key", "provider", p.ID)
				c.Providers.Del(string(p.ID))
			}
			continue
		}
		c.Providers.Set(string(p.ID), prepared)
	}

	ownerReferences := make(map[string]ProviderOwnerReference)
	pluginOwnership := make(map[string]ProviderPluginReference)
	presetOwnership := make(map[string]ProviderPresetReference)
	for id, provider := range c.Providers.Seq2() {
		provider = c.completeProviderOwner(id, provider)
		c.Providers.Set(id, provider)
		if provider.Owner != nil {
			ownerReferences[id] = *provider.Owner
		}
		if provider.Plugin != nil {
			pluginOwnership[id] = *provider.Plugin
		} else if provider.Preset != nil {
			presetOwnership[id] = *provider.Preset
		}
	}
	if migrate != nil {
		if err := migrate(ownerReferences, pluginOwnership, presetOwnership); err != nil {
			return fmt.Errorf("migrate provider ownership: %w", err)
		}
	}

	// Discover models concurrently for custom providers that need it.
	// A provider needs discovery when discover_models is explicitly true,
	// or when the models list is empty (auto-trigger, unless opted out).
	type discoveryResult struct {
		models []catalog.Model
		err    error
	}

	discoveryResults := make(map[string]discoveryResult)
	var mu sync.Mutex
	var wg sync.WaitGroup

	discoverCtx, discoverCancel := context.WithTimeout(ctx, 3*time.Second)
	for id, pc := range c.Providers.Seq2() {
		if knownProviderNames[id] || pc.Plugin != nil || c.isUnavailableRegisteredProvider(id) {
			continue
		}
		if pc.Disable || pc.BaseURL == "" {
			continue
		}
		wantsDiscovery := pc.AutoDiscoverModels != nil && *pc.AutoDiscoverModels
		autoTrigger := len(pc.Models) == 0 && (pc.AutoDiscoverModels == nil || *pc.AutoDiscoverModels)
		if !wantsDiscovery && !autoTrigger {
			continue
		}
		providerID := cmp.Or(pc.ID, id)
		cfg := discover.Config{
			ID:             providerID,
			BaseURL:        pc.BaseURL,
			APIKey:         pc.APIKey,
			ExtraHeaders:   pc.ExtraHeaders,
			ExistingModels: pc.Models,
		}
		providerType := cmp.Or(pc.Type, catalog.TypeOpenAICompat)
		wg.Go(func() {
			models, err := discover.DiscoverModels(discoverCtx, cfg, resolver)
			if err == nil && len(models) > 0 {
				if enricher := discover.GetEnricher(string(providerType)); enricher != nil {
					models, _ = enricher.EnrichModels(discoverCtx, cfg, resolver, models)
				}
			}
			mu.Lock()
			discoveryResults[id] = discoveryResult{models: models, err: err}
			mu.Unlock()
		})
	}
	wg.Wait()
	discoverCancel()

	// Validate the custom providers.
	for id, providerConfig := range c.Providers.Seq2() {
		if knownProviderNames[id] {
			continue
		}
		if providerConfig.Plugin != nil || c.isUnavailableRegisteredProvider(id) {
			providerConfig.ID = id
			providerConfig.Name = cmp.Or(providerConfig.Name, id)
			c.Providers.Set(id, providerConfig)
			continue
		}

		// Make sure the provider ID is set.
		providerConfig.ID = id
		providerConfig.Name = cmp.Or(providerConfig.Name, id) // Use ID as name if not set
		// Empty and local custom-provider types all use the retained
		// OpenAI-compatible runtime.
		providerConfig.Type = cmp.Or(providerConfig.Type, catalog.TypeOpenAICompat)
		if providerConfig.Type != catalog.TypeOpenAICompat &&
			!discover.IsKnownCustomProvider(string(providerConfig.Type)) {
			slog.Warn("Skipping custom provider due to unsupported provider type", "provider", id)
			c.Providers.Del(id)
			continue
		}

		if providerConfig.Disable {
			slog.Debug("Custom provider disabled, preserving in config", "provider", id)
			c.Providers.Set(id, providerConfig)
			continue
		}
		if providerConfig.APIKey == "" {
			slog.Warn("Provider is missing API key, this might be OK for local providers", "provider", id)
		}
		if providerConfig.BaseURL == "" {
			slog.Warn("Skipping custom provider due to missing API endpoint", "provider", id)
			c.Providers.Del(id)
			continue
		}

		// Apply discovery results if available.
		if result, ok := discoveryResults[id]; ok {
			if result.err != nil {
				slog.Warn("Model discovery failed", "provider", id, "error", result.err)
				if len(providerConfig.Models) == 0 {
					slog.Warn("Skipping provider with no models after failed discovery", "provider", id)
					c.Providers.Del(id)
					continue
				}
			} else if len(result.models) > 0 {
				providerConfig.Models = result.models
				slog.Info("Discovered models for provider", "provider", id, "count", len(result.models))
			}
		}

		if len(providerConfig.Models) == 0 {
			slog.Warn("Skipping custom provider because the provider has no models", "provider", id)
			c.Providers.Del(id)
			continue
		}

		apiKey, err := resolver.ResolveValue(providerConfig.APIKey)
		if apiKey == "" || err != nil {
			slog.Warn("Provider is missing API key, this might be OK for local providers", "provider", id)
		}
		baseURL, err := resolver.ResolveValue(providerConfig.BaseURL)
		if baseURL == "" || err != nil {
			slog.Warn("Skipping custom provider due to missing API endpoint", "provider", id, "error", err)
			c.Providers.Del(id)
			continue
		}

		// Custom-provider headers share the MCP error contract; see
		// the known-provider loop above.
		for k, v := range providerConfig.ExtraHeaders {
			resolved, err := resolver.ResolveValue(v)
			if err != nil {
				return fmt.Errorf("resolving provider %s header %q: %w", id, k, err)
			}
			if resolved == "" {
				delete(providerConfig.ExtraHeaders, k)
				continue
			}
			providerConfig.ExtraHeaders[k] = resolved
		}

		c.Providers.Set(id, providerConfig)
	}

	if c.Providers.Len() == 0 && c.Options.DisableDefaultProviders {
		return fmt.Errorf("default providers are disabled and there are no custom providers are configured")
	}

	return nil
}

var immutableHostEnvironmentVariables = map[string]bool{
	"AI_CLI_DIR":                     true,
	"CRUX_CACHE_DIR":                 true,
	"CRUX_DISABLE_DEFAULT_PROVIDERS": true,
	"CRUX_GLOBAL_CONFIG":             true,
	"CRUX_GLOBAL_DATA":               true,
	"CRUX_PROVIDER_PLUGIN_COMPAT":    true,
	"CRUX_PROVIDER_PLUGINS":          true,
	"CRUX_PROVIDER_PROFILE":          true,
	"CRUX_SKILLS_DIR":                true,
	"HOME":                           true,
	"HOMEDRIVE":                      true,
	"HOMEPATH":                       true,
	"LOCALAPPDATA":                   true,
	"USERPROFILE":                    true,
	"XDG_CACHE_HOME":                 true,
	"XDG_CONFIG_HOME":                true,
	"XDG_DATA_HOME":                  true,
}

func snapshotEnvironment() env.Env {
	return cloneEnvironment(env.New())
}

func cloneEnvironment(environment env.Env) env.Env {
	return env.NewFromMap(environmentValues(environment))
}

func environmentValues(environment env.Env) map[string]string {
	values := make(map[string]string)
	if environment == nil {
		return values
	}
	for _, entry := range environment.Env() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func (c *Config) buildEnvironment() (env.Env, VariableResolver, map[string]string, error) {
	return c.buildEnvironmentFrom(snapshotEnvironment())
}

func (c *Config) buildEnvironmentFrom(base env.Env) (env.Env, VariableResolver, map[string]string, error) {
	values := environmentValues(base)
	candidate := env.NewFromMap(values)
	resolver := NewShellVariableResolver(candidate)
	resolvedValues := make(map[string]string, len(c.Env))
	keys := make([]string, 0, len(c.Env))
	for key := range c.Env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return nil, nil, nil, fmt.Errorf("invalid environment variable name %q", key)
		}
		if immutableHostEnvironmentVariables[key] {
			return nil, nil, nil, fmt.Errorf("environment variable %q is a startup-only host setting", key)
		}
		resolved, err := resolver.ResolveValue(c.Env[key])
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolve environment variable %q", key)
		}
		if strings.ContainsRune(resolved, '\x00') {
			return nil, nil, nil, fmt.Errorf("environment variable %q contains an invalid value", key)
		}
		values[key] = resolved
		resolvedValues[key] = resolved
	}
	return candidate, resolver, resolvedValues, nil
}

func applyEnvironment(base env.Env, previous, current map[string]string) error {
	return applyEnvironmentWith(base, previous, current, os.Setenv, os.Unsetenv)
}

func applyEnvironmentWith(base env.Env, previous, current map[string]string, setenv func(string, string) error, unsetenv func(string) error) error {
	baseValues := environmentValues(base)
	keys := make([]string, 0, len(previous)+len(current))
	seen := make(map[string]bool, len(previous)+len(current))
	for key := range previous {
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	for key := range current {
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	slices.Sort(keys)
	type originalValue struct {
		value  string
		exists bool
	}
	originals := make(map[string]originalValue, len(keys))
	applied := make([]string, 0, len(keys))
	for _, key := range keys {
		value, exists := os.LookupEnv(key)
		originals[key] = originalValue{value: value, exists: exists}
		var err error
		if value, ok := current[key]; ok {
			err = setenv(key, value)
		} else if value, ok := baseValues[key]; ok {
			err = setenv(key, value)
		} else {
			err = unsetenv(key)
		}
		if err == nil {
			applied = append(applied, key)
			continue
		}
		result := fmt.Errorf("apply environment variable %q: %w", key, err)
		for index := len(applied) - 1; index >= 0; index-- {
			appliedKey := applied[index]
			original := originals[appliedKey]
			if original.exists {
				err = setenv(appliedKey, original.value)
			} else {
				err = unsetenv(appliedKey)
			}
			if err != nil {
				result = errors.Join(result, fmt.Errorf("restore environment variable %q: %w", appliedKey, err))
			}
		}
		return result
	}
	return nil
}

func (c *Config) setDefaults(workingDir, dataDir string) {
	_ = c.setDefaultsFromEnvironment(workingDir, dataDir, env.New())
}

func (c *Config) setDefaultsFromEnvironment(workingDir, dataDir string, environment env.Env) error {
	if err := c.Images.Validate(); err != nil {
		return err
	}
	if environment == nil {
		environment = env.New()
	}
	if c.Options == nil {
		c.Options = &Options{}
	}
	if c.Options.TUI == nil {
		c.Options.TUI = &TUIOptions{}
	}
	if len(c.Options.GlobalContextPaths) == 0 {
		cruxConfigDir := filepath.Dir(globalConfigFromEnvironment(environment))
		c.Options.GlobalContextPaths = []string{
			filepath.Join(cruxConfigDir, "CRUX.md"),
			filepath.Join(filepath.Dir(cruxConfigDir), "AGENTS.md"),
		}
	}
	slices.Sort(c.Options.GlobalContextPaths)
	c.Options.GlobalContextPaths = slices.Compact(c.Options.GlobalContextPaths)

	if dataDir != "" {
		c.Options.DataDirectory = dataDir
	} else if c.Options.DataDirectory == "" {
		if path, ok := fsext.LookupClosestBounded(workingDir, projectBoundary(workingDir), defaultDataDirectory); ok {
			c.Options.DataDirectory = path
		} else {
			c.Options.DataDirectory = filepath.Join(workingDir, defaultDataDirectory)
		}
	}
	c.Options.DataDirectory = filepath.Clean(filepathext.SmartJoin(workingDir, c.Options.DataDirectory))
	if c.Providers == nil {
		c.Providers = csync.NewMap[string, ProviderConfig]()
	}
	if c.Models == nil {
		c.Models = make(map[SelectedModelType]SelectedModel)
	}
	if c.RecentModels == nil {
		c.RecentModels = make(map[SelectedModelType][]SelectedModel)
	}
	if c.MCP == nil {
		c.MCP = make(map[string]MCPConfig)
	}
	// Drop orphaned OAuth token entries left behind when a user removes
	// an MCP from crux.json. See MCPConfig.isOrphanedToken.
	for name, m := range c.MCP {
		if m.isOrphanedToken() {
			delete(c.MCP, name)
		}
	}
	if c.LSP == nil {
		c.LSP = make(map[string]LSPConfig)
	}

	// Apply defaults to LSP configurations
	c.applyLSPDefaults()

	// Add the default context paths if they are not already present.
	c.Options.ContextPaths = append(slices.Clone(defaultContextPaths), c.Options.ContextPaths...)

	// Prepend ~/.ai-cli/ per-project instructions (absolute path, resolved
	// from the working directory hash). This is the primary instructions
	// source; the AGENTS.md/CRUX.md files above serve as fallbacks.
	if projInstr := aiCliProjectInstructionsPathFromEnvironment(workingDir, environment); projInstr != "" {
		c.Options.ContextPaths = append([]string{projInstr}, c.Options.ContextPaths...)
	}

	slices.Sort(c.Options.ContextPaths)
	c.Options.ContextPaths = slices.Compact(c.Options.ContextPaths)

	// Add the default skills directories if not already present.
	for _, dir := range globalSkillsDirsFromEnvironment(environment) {
		if !slices.Contains(c.Options.SkillsPaths, dir) {
			c.Options.SkillsPaths = append(c.Options.SkillsPaths, dir)
		}
	}

	// Project specific skills dirs.
	c.Options.SkillsPaths = append(c.Options.SkillsPaths, ProjectSkillsDir(workingDir)...)

	if str, ok := environmentValues(environment)["CRUX_DISABLE_DEFAULT_PROVIDERS"]; ok {
		disabled, err := strconv.ParseBool(str)
		if err != nil {
			return fmt.Errorf("invalid CRUX_DISABLE_DEFAULT_PROVIDERS value %q: %w", str, err)
		}
		c.Options.DisableDefaultProviders = disabled
	}

	// /init and the startup initialization flow target the per-project
	// ai-cli instructions file: it is the primary context injection source
	// in this fork, while AGENTS.md is only a fallback read.
	c.Options.InitializeAs = cmp.Or(c.Options.InitializeAs, aiCliProjectInstructionsPathFromEnvironment(workingDir, environment), defaultInitializeAs)
	return nil
}

// powernapDefaults caches the powernap default LSP server catalog. The
// catalog is static and immutable for the life of the process, but
// building it (NewManager + LoadDefaults) is expensive and was previously
// repeated on every config reload. We load it once and only ever read from
// it via GetServer, so a shared instance is safe.
var (
	powernapDefaultsOnce sync.Once
	powernapDefaults     *powernapConfig.Manager
)

func lspDefaultsManager() *powernapConfig.Manager {
	powernapDefaultsOnce.Do(func() {
		m := powernapConfig.NewManager()
		// LoadDefaults only fails on malformed embedded defaults, which
		// would be a build-time bug; treat the manager as usable either
		// way so a transient error never wedges config loading.
		_ = m.LoadDefaults()
		powernapDefaults = m
	})
	return powernapDefaults
}

// applyLSPDefaults applies default values from powernap to LSP configurations
func (c *Config) applyLSPDefaults() {
	// Reuse the process-wide default catalog; building it per reload was a
	// significant chunk of reload latency.
	configManager := lspDefaultsManager()

	// Apply defaults to each LSP configuration
	for name, cfg := range c.LSP {
		// Try to get defaults from powernap based on name or command name.
		base, ok := configManager.GetServer(name)
		if !ok {
			base, ok = configManager.GetServer(cfg.Command)
			if !ok {
				continue
			}
		}
		if cfg.Options == nil {
			cfg.Options = base.Settings
		}
		if cfg.InitOptions == nil {
			cfg.InitOptions = base.InitOptions
		}
		if len(cfg.FileTypes) == 0 {
			cfg.FileTypes = base.FileTypes
		}
		if len(cfg.RootMarkers) == 0 {
			cfg.RootMarkers = base.RootMarkers
		}
		cfg.Command = cmp.Or(cfg.Command, base.Command)
		if len(cfg.Args) == 0 {
			cfg.Args = base.Args
		}
		if len(cfg.Env) == 0 {
			cfg.Env = base.Environment
		}
		// Update the config in the map
		c.LSP[name] = cfg
	}
}

// defaultModelSelection chooses defaults only when no valid explicit selection
// owns a model slot. Its result must never replace an explicit selection merely
// because a plugin is temporarily unregistered, disabled by rollout, or absent
// from this process. That selection remains user-owned configuration.
func (c *Config) defaultModelSelection(knownProviders []catalog.Provider) (largeModel SelectedModel, smallModel SelectedModel, err error) {
	if len(knownProviders) == 0 && c.Providers.Len() == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return largeModel, smallModel, err
	}

	// Use the first provider enabled based on the known providers order
	// if no provider found that is known use the first provider configured
	for _, p := range knownProviders {
		providerConfig, ok := c.Providers.Get(string(p.ID))
		if !ok || !c.IsProviderAvailable(string(p.ID)) {
			continue
		}
		defaultLargeModel := c.GetModel(string(p.ID), p.DefaultLargeModelID)
		if defaultLargeModel == nil {
			slog.Warn("Default large model %s not found for provider %s", p.DefaultLargeModelID, p.ID)
			if len(providerConfig.Models) == 0 {
				return largeModel, smallModel, fmt.Errorf("default large model %s not found for provider %s", p.DefaultLargeModelID, p.ID)
			}
			defaultLargeModel = &providerConfig.Models[0]
		}
		largeModel = SelectedModel{
			Provider:        string(p.ID),
			Model:           defaultLargeModel.ID,
			MaxTokens:       defaultLargeModel.DefaultMaxTokens,
			ReasoningEffort: defaultLargeModel.DefaultReasoningEffort,
		}

		defaultSmallModel := c.GetModel(string(p.ID), p.DefaultSmallModelID)
		if defaultSmallModel == nil {
			slog.Warn("Default small model %s not found for provider %s", p.DefaultSmallModelID, p.ID)
			if len(providerConfig.Models) == 0 {
				return largeModel, smallModel, fmt.Errorf("default small model %s not found for provider %s", p.DefaultSmallModelID, p.ID)
			}
			defaultSmallModel = &providerConfig.Models[0]
		}
		smallModel = SelectedModel{
			Provider:        string(p.ID),
			Model:           defaultSmallModel.ID,
			MaxTokens:       defaultSmallModel.DefaultMaxTokens,
			ReasoningEffort: defaultSmallModel.DefaultReasoningEffort,
		}
		return largeModel, smallModel, err
	}

	enabledProviders := c.EnabledProviders()
	slices.SortFunc(enabledProviders, func(a, b ProviderConfig) int {
		return strings.Compare(a.ID, b.ID)
	})

	if len(enabledProviders) == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return largeModel, smallModel, err
	}

	providerConfig := enabledProviders[0]
	if len(providerConfig.Models) == 0 {
		err = fmt.Errorf("provider %s has no models configured", providerConfig.ID)
		return largeModel, smallModel, err
	}
	defaultLargeModel := c.GetModel(providerConfig.ID, providerConfig.Models[0].ID)
	largeModel = SelectedModel{
		Provider:  providerConfig.ID,
		Model:     defaultLargeModel.ID,
		MaxTokens: defaultLargeModel.DefaultMaxTokens,
	}
	defaultSmallModel := c.GetModel(providerConfig.ID, providerConfig.Models[0].ID)
	smallModel = SelectedModel{
		Provider:  providerConfig.ID,
		Model:     defaultSmallModel.ID,
		MaxTokens: defaultSmallModel.DefaultMaxTokens,
	}
	return largeModel, smallModel, err
}

// resolvedModels holds the result of resolving user-configured model
// selections against the provider catalog.
type resolvedModels struct {
	Large         SelectedModel
	Small         SelectedModel
	LargeFallback bool // true if Large was corrected to a default
	SmallFallback bool // true if Small was corrected to a default
}

// resolveSelectedModels is the authority for installing persisted model
// selections into the runtime configuration at startup and reload. A selected
// owned provider that is temporarily unavailable is retained byte-for-value,
// including model identity, token limits, reasoning controls, sampling values,
// penalties, and provider options. Unavailability is not invalidity and must
// never select an unrelated provider as a hidden runtime substitute.
//
// Fallback is permitted only for a selection whose integration is available
// but whose model ID is invalid. The fallback flags authorize callers to
// persist that correction. Unavailable selections never set those flags. Keep
// this function pure: callers own assignment and persistence.
func (c *Config) defaultModelForProvider(providerID string, modelType SelectedModelType, knownProviders []catalog.Provider) (SelectedModel, error) {
	provider, ok := c.Providers.Get(providerID)
	if !ok || !c.IsProviderAvailable(providerID) {
		return SelectedModel{}, fmt.Errorf("selected provider %s is not available", providerID)
	}
	if len(provider.Models) == 0 {
		return SelectedModel{}, fmt.Errorf("selected provider %s has no models configured", providerID)
	}

	defaultModelID := ""
	for _, known := range knownProviders {
		if string(known.ID) != providerID {
			continue
		}
		if modelType == SelectedModelTypeSmall {
			defaultModelID = known.DefaultSmallModelID
		} else {
			defaultModelID = known.DefaultLargeModelID
		}
		break
	}
	model := c.GetModel(providerID, defaultModelID)
	if model == nil {
		model = &provider.Models[0]
	}
	return SelectedModel{
		Provider:        providerID,
		Model:           model.ID,
		MaxTokens:       model.DefaultMaxTokens,
		ReasoningEffort: model.DefaultReasoningEffort,
	}, nil
}

func resolveSelectedModels(cfg *Config, knownProviders []catalog.Provider) (resolvedModels, error) {
	var result resolvedModels
	largeModelSelected, largeModelConfigured := cfg.Models[SelectedModelTypeLarge]
	smallModelSelected, smallModelConfigured := cfg.Models[SelectedModelTypeSmall]
	largeUnavailable := largeModelConfigured && cfg.isUnavailableRegisteredProvider(largeModelSelected.Provider)
	smallUnavailable := smallModelConfigured && cfg.isUnavailableRegisteredProvider(smallModelSelected.Provider)

	defaultLarge, _, err := cfg.defaultModelSelection(knownProviders)
	if err != nil {
		switch {
		case largeUnavailable && smallUnavailable:
			result.Large = largeModelSelected
			result.Small = smallModelSelected
			return result, nil
		case largeUnavailable:
			result.Large = largeModelSelected
			if smallModelConfigured {
				result.Small = smallModelSelected
			} else {
				result.Small = largeModelSelected
			}
			return result, nil
		case smallUnavailable:
			result.Small = smallModelSelected
			if largeModelConfigured {
				result.Large = largeModelSelected
			} else {
				result.Large = smallModelSelected
			}
			return result, nil
		default:
			return result, fmt.Errorf("failed to select default models: %w", err)
		}
	}

	resolve := func(modelType SelectedModelType, selected SelectedModel, configured, unavailable bool, fallback SelectedModel) (SelectedModel, bool, error) {
		if unavailable {
			return selected, false, nil
		}
		if !configured {
			return fallback, false, nil
		}

		providerID := cmp.Or(selected.Provider, fallback.Provider)
		modelID := cmp.Or(selected.Model, fallback.Model)
		model := cfg.GetModel(providerID, modelID)
		if model == nil {
			if selected.Provider == "" {
				return fallback, true, nil
			}
			providerFallback, fallbackErr := cfg.defaultModelForProvider(selected.Provider, modelType, knownProviders)
			if fallbackErr != nil {
				return SelectedModel{}, false, fallbackErr
			}
			return providerFallback, true, nil
		}

		resolved := SelectedModel{
			Provider:        providerID,
			Model:           modelID,
			MaxTokens:       model.DefaultMaxTokens,
			ReasoningEffort: model.DefaultReasoningEffort,
			Think:           selected.Think,
		}
		if selected.MaxTokens > 0 {
			resolved.MaxTokens = selected.MaxTokens
		}
		if selected.ReasoningEffort != "" {
			resolved.ReasoningEffort = selected.ReasoningEffort
		}
		resolved.Temperature = selected.Temperature
		resolved.TopP = selected.TopP
		resolved.TopK = selected.TopK
		resolved.FrequencyPenalty = selected.FrequencyPenalty
		resolved.PresencePenalty = selected.PresencePenalty
		resolved.ProviderOptions = maps.Clone(selected.ProviderOptions)
		return resolved, false, nil
	}

	large, largeFallback, err := resolve(SelectedModelTypeLarge, largeModelSelected, largeModelConfigured, largeUnavailable, defaultLarge)
	if err != nil {
		return result, fmt.Errorf("resolve selected large model: %w", err)
	}

	var smallDefault SelectedModel
	switch {
	case smallUnavailable:
	case smallModelSelected.Provider != "":
		smallDefault, err = cfg.defaultModelForProvider(smallModelSelected.Provider, SelectedModelTypeSmall, knownProviders)
		if err != nil {
			return result, fmt.Errorf("resolve default small model: %w", err)
		}
	case largeUnavailable:
		if !smallModelConfigured {
			smallModelSelected = large
			smallModelConfigured = true
		} else {
			smallModelSelected.Provider = large.Provider
			if smallModelSelected.Model == "" {
				smallModelSelected.Model = large.Model
			}
		}
		smallUnavailable = true
	default:
		smallDefault, err = cfg.defaultModelForProvider(large.Provider, SelectedModelTypeSmall, knownProviders)
		if err != nil {
			return result, fmt.Errorf("resolve default small model: %w", err)
		}
	}

	small, smallFallback, err := resolve(SelectedModelTypeSmall, smallModelSelected, smallModelConfigured, smallUnavailable, smallDefault)
	if err != nil {
		return result, fmt.Errorf("resolve selected small model: %w", err)
	}

	result.Large = large
	result.Small = small
	result.LargeFallback = largeFallback
	result.SmallFallback = smallFallback
	return result, nil
}

// isUnavailableRegisteredProvider distinguishes a retained exact owner or
// legacy plugin, preset, or OAuth owner from an ordinary custom provider when
// its exact integration is absent. This classification protects the retained
// configuration from generic-provider normalization and model fallback. Do not
// weaken it to same-ID registration health alone or remove the durable
// ownership checks.
func (c *Config) isUnavailableRegisteredProvider(providerID string) bool {
	provider, ok := c.Providers.Get(providerID)
	if !ok {
		return false
	}
	if provider.Owner != nil {
		_, active := providerOwnerForProvider(c, c.providerCapabilities(), providerID, provider)
		return !active
	}
	if provider.Preset != nil {
		_, active := c.providerPreset(c.providerCapabilities(), providerID)
		return !active
	}
	if provider.Plugin != nil {
		_, active := c.providerRegistration(c.providerCapabilities(), providerID)
		return !active
	}
	if provider.OAuthToken == nil || provider.Type == catalog.TypeOpenAICompat {
		return false
	}
	registration, registered := c.providerCapabilities().Lookup(providerID)
	return !registered || registration.ProviderID != providerID
}

// lookupConfigs searches config files starting at cwd and walking up
// through the current project. The upward walk stops at the git
// working tree root when one can be detected, otherwise at cwd itself,
// so an unrelated crux.json placed above the project is never picked
// up. Global user-level config locations are always included
// regardless of the boundary.
func lookupConfigs(cwd string) []string {
	return lookupConfigsFromEnvironment(cwd, env.New())
}

func lookupConfigsFromEnvironment(cwd string, environment env.Env) []string {
	globalConfigPath := globalConfigFromEnvironment(environment)
	// Prepend global user config and machine-owned data JSON. Only the user
	// config directory contributes a cruxrc; the data directory is writable
	// machine state and must never be executed as Bash. Missing files are
	// skipped when loaded.
	configPaths := []string{
		systemConfigPath,
		globalConfigPath,
		shellConfigSibling(globalConfigPath),
		globalConfigDataFromEnvironment(appName, environment),
	}

	// Ordered high-to-low priority within a directory. LookupBounded returns
	// matches in this order, and the later reverse + merge make the earliest
	// listed name win on conflict. So: .cruxrc beats cruxrc, both beat the
	// JSON configs, and .crux.json beats crux.json.
	configNames := []string{
		"." + appName + "rc",
		appName + "rc",
		"." + appName + ".json",
		appName + ".json",
	}

	foundConfigs, err := fsext.LookupBounded(cwd, projectBoundary(cwd), configNames...)
	if err != nil {
		// returns at least default configs
		return configPaths
	}

	// reverse order so last config has more priority
	slices.Reverse(foundConfigs)

	return append(configPaths, foundConfigs...)
}

func loadFromConfigPaths(ctx context.Context, configPaths []string) (*Config, []string, error) {
	return loadFromConfigPathsWithOverrides(ctx, configPaths, nil)
}

func loadFromConfigPathsWithOverrides(ctx context.Context, configPaths []string, overrides map[string][]byte, environments ...env.Env) (*Config, []string, error) {
	environment := env.New()
	if len(environments) > 0 && environments[0] != nil {
		environment = environments[0]
	}
	var configs [][]byte
	var loaded []string

	// Track directories that have both crux.json and cruxrc to warn
	// about potential confusion, along with the top-level keys each
	// defines so we can report conflicts.
	jsonDirKeys := make(map[string]map[string]bool)
	shDirKeys := make(map[string]map[string]bool)

	for _, path := range configPaths {
		if path == "" {
			continue
		}
		data, overridden := overrides[path]
		var err error
		if !overridden {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("failed to open config file %s: %w", path, err)
		}
		if len(data) == 0 {
			continue
		}

		dir := filepath.Dir(path)
		if isShellConfig(path) {
			jsonBytes, err := shellconfig.LoadShellConfig(ctx, path, data, environment.Env())
			if err != nil {
				return nil, nil, fmt.Errorf("failed to load shell config %s: %w", path, err)
			}
			if len(jsonBytes) > 0 {
				if !json.Valid(jsonBytes) {
					return nil, nil, fmt.Errorf("shell config %s produced invalid JSON", path)
				}
				addTopLevelKeys(shDirKeys, dir, jsonBytes)
				configs = append(configs, jsonBytes)
				loaded = append(loaded, path)
			}
		} else {
			if !json.Valid(data) {
				return nil, nil, fmt.Errorf("invalid JSON in config file %s", path)
			}
			addTopLevelKeys(jsonDirKeys, dir, data)
			configs = append(configs, data)
			loaded = append(loaded, path)
		}
	}

	// Warn if both a JSON config and a cruxrc exist in the same directory
	// and define overlapping top-level keys. Disjoint coexistence is
	// intentional and not worth warning about.
	for dir, jKeys := range jsonDirKeys {
		sKeys, ok := shDirKeys[dir]
		if !ok {
			continue
		}
		var conflicts []string
		for k := range jKeys {
			if sKeys[k] {
				conflicts = append(conflicts, k)
			}
		}
		if len(conflicts) > 0 {
			slices.Sort(conflicts)
			slog.Warn("Found both a JSON config and a cruxrc in the same directory; merging with cruxrc taking precedence",
				"dir", dir, "conflicting_keys", strings.Join(conflicts, ", "))
		}
	}

	cfg, err := loadFromBytes(configs)
	if err != nil {
		return nil, nil, err
	}
	return cfg, loaded, nil
}

// addTopLevelKeys records the top-level JSON keys present in data into the
// set for dir.
func addTopLevelKeys(m map[string]map[string]bool, dir string, data []byte) {
	keys := m[dir]
	if keys == nil {
		keys = make(map[string]bool)
		m[dir] = keys
	}
	gjson.ParseBytes(data).ForEach(func(key, _ gjson.Result) bool {
		keys[key.String()] = true
		return true
	})
}

func loadFromBytes(configs [][]byte) (*Config, error) {
	if len(configs) == 0 {
		return &Config{}, nil
	}

	data, err := jsons.Merge(configs)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

// migrateDisableNotifications migrates the deprecated disable_notifications
// and notification_style fields to the unified notifications field. It checks
// both the user config (~/.ai-cli) and data config (~/.ai-cli/data) files. If
// disable_notifications is true, it sets notifications to "disabled" in the
// data file. If notification_style is set, it moves the value to notifications.
// Regardless of value, it removes the deprecated fields from any file that
// contains them.
type configPreimage struct {
	path           string
	data           []byte
	exists         bool
	expectedData   []byte
	expectedExists bool
}

func captureConfigPreimages(paths ...string) ([]configPreimage, error) {
	result := make([]configPreimage, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		exists := err == nil
		result = append(result, configPreimage{
			path:           path,
			data:           data,
			exists:         exists,
			expectedData:   slices.Clone(data),
			expectedExists: exists,
		})
	}
	return result, nil
}

func configPreimageChanged(preimages []configPreimage, path string) (bool, error) {
	for _, preimage := range preimages {
		if preimage.path != path {
			continue
		}
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return preimage.exists, nil
		}
		if err != nil {
			return false, err
		}
		return !preimage.exists || string(data) != string(preimage.data), nil
	}
	return false, nil
}

func captureConfigPostimages(preimages []configPreimage) error {
	var result error
	for index := range preimages {
		data, err := os.ReadFile(preimages[index].path)
		if os.IsNotExist(err) {
			preimages[index].expectedData = nil
			preimages[index].expectedExists = false
			continue
		}
		if err != nil {
			result = errors.Join(result, fmt.Errorf("read %s after correction: %w", preimages[index].path, err))
			continue
		}
		preimages[index].expectedData = data
		preimages[index].expectedExists = true
	}
	return result
}

func configPostimage(preimages []configPreimage, path string) ([]byte, bool, error) {
	for _, preimage := range preimages {
		if preimage.path == path {
			return slices.Clone(preimage.expectedData), preimage.expectedExists, nil
		}
	}
	return nil, false, fmt.Errorf("config postimage for %s was not captured", path)
}

func recordConfigPostimage(preimages []configPreimage, path string, data []byte) error {
	for index := range preimages {
		if preimages[index].path == path {
			preimages[index].expectedData = slices.Clone(data)
			preimages[index].expectedExists = true
			return nil
		}
	}
	return fmt.Errorf("config preimage for %s was not captured", path)
}

func restoreConfigPreimages(preimages []configPreimage) error {
	var result error
	for _, preimage := range preimages {
		if err := os.MkdirAll(filepath.Dir(preimage.path), 0o755); err != nil {
			result = errors.Join(result, fmt.Errorf("create restore directory for %s: %w", preimage.path, err))
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), configLockDeadline)
		release, err := lock.File(ctx, preimage.path+".lock")
		cancel()
		if err != nil {
			result = errors.Join(result, fmt.Errorf("lock %s for restore: %w", preimage.path, err))
			continue
		}
		current, readErr := os.ReadFile(preimage.path)
		currentExists := readErr == nil
		if os.IsNotExist(readErr) {
			readErr = nil
		}
		if readErr != nil {
			result = errors.Join(result, fmt.Errorf("read %s before restore: %w", preimage.path, readErr))
			release()
			continue
		}
		if currentExists != preimage.expectedExists || !slices.Equal(current, preimage.expectedData) {
			release()
			continue
		}
		if preimage.exists {
			err = atomicWriteFile(preimage.path, preimage.data, 0o600)
		} else {
			err = os.Remove(preimage.path)
			if os.IsNotExist(err) {
				err = nil
			}
		}
		release()
		if err != nil {
			result = errors.Join(result, fmt.Errorf("restore %s: %w", preimage.path, err))
		}
	}
	return result
}

type notificationMigrationPlan struct {
	overrides        map[string][]byte
	cleanPaths       map[string]bool
	value            string
	dataConfig       string
	setNotifications bool
}

func prepareDisableNotificationsMigration(globalConfig, dataConfig string) notificationMigrationPlan {
	originals := make(map[string][]byte)
	filesToClean := make([]string, 0, 2)
	var wasDisabled bool
	var styleValue string
	for _, path := range []string{globalConfig, dataConfig} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		originals[path] = data
		needsClean := false
		if gjson.GetBytes(data, "options.disable_notifications").Exists() {
			needsClean = true
			if gjson.GetBytes(data, "options.disable_notifications").Bool() {
				wasDisabled = true
			}
		}
		if value := gjson.GetBytes(data, "options.notification_style"); value.Exists() {
			needsClean = true
			if styleValue == "" {
				styleValue = value.String()
			}
		}
		if needsClean {
			filesToClean = append(filesToClean, path)
		}
	}
	plan := notificationMigrationPlan{overrides: make(map[string][]byte), cleanPaths: make(map[string]bool), value: styleValue, dataConfig: dataConfig}
	if plan.value == "" && wasDisabled {
		plan.value = "disabled"
	}
	if plan.value != "" {
		if data, ok := originals[dataConfig]; ok && !gjson.GetBytes(data, "options.notifications").Exists() {
			if updated, err := sjson.SetBytes(data, "options.notifications", plan.value); err == nil {
				plan.overrides[dataConfig] = updated
				plan.setNotifications = true
			}
		}
	}
	for _, path := range filesToClean {
		plan.cleanPaths[path] = true
		data := originals[path]
		if updated, ok := plan.overrides[path]; ok {
			data = updated
		}
		updated, _ := sjson.DeleteBytes(data, "options.disable_notifications")
		updated, _ = sjson.DeleteBytes(updated, "options.notification_style")
		if string(updated) != string(originals[path]) {
			plan.overrides[path] = updated
		}
	}
	return plan
}

func (p notificationMigrationPlan) apply(path string, data []byte) ([]byte, error) {
	value := string(data)
	var err error
	if p.setNotifications && path == p.dataConfig {
		value, err = sjson.Set(value, "options.notifications", p.value)
		if err != nil {
			return nil, err
		}
	}
	if p.cleanPaths[path] {
		value, err = sjson.Delete(value, "options.disable_notifications")
		if err != nil {
			return nil, err
		}
		value, err = sjson.Delete(value, "options.notification_style")
		if err != nil {
			return nil, err
		}
	}
	return []byte(value), nil
}

var (
	inspectStartupConfigPreimageChanged = configPreimageChanged
	writeStartupConfigFile              = atomicWriteFile
)

func commitStartupCorrections(store *ConfigStore, notification notificationMigrationPlan, modelFields map[string]any, preimages []configPreimage) error {
	paths := make(map[string]bool, len(notification.overrides)+1)
	for path := range notification.overrides {
		paths[path] = true
	}
	if len(modelFields) > 0 {
		paths[store.globalDataPath] = true
	}
	orderedPaths := make([]string, 0, len(paths))
	for path := range paths {
		orderedPaths = append(orderedPaths, path)
	}
	slices.Sort(orderedPaths)
	if paths[store.globalDataPath] {
		orderedPaths = append(slices.DeleteFunc(orderedPaths, func(path string) bool { return path == store.globalDataPath }), store.globalDataPath)
	}
	for _, path := range orderedPaths {
		var unlock func()
		if path == store.globalDataPath {
			var err error
			unlock, err = store.lockConfig(ScopeGlobal)
			if err != nil {
				return err
			}
		}
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			data = []byte("{}")
			err = nil
		}
		if err == nil {
			data, err = notification.apply(path, data)
		}
		if err == nil && path == store.globalDataPath {
			keys := make([]string, 0, len(modelFields))
			for key := range modelFields {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			value := string(data)
			for _, key := range keys {
				value, err = sjson.Set(value, key, modelFields[key])
				if err != nil {
					break
				}
			}
			data = []byte(value)
		}
		if err == nil {
			err = writeStartupConfigFile(path, data, 0o600)
		}
		if err == nil {
			err = recordConfigPostimage(preimages, path, data)
		}
		if unlock != nil {
			unlock()
		}
		if err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

func (p notificationMigrationPlan) commit() {
	for path, data := range p.overrides {
		if err := atomicWriteFile(path, data, 0o600); err != nil {
			slog.Warn("Failed to write migrated notification config", "path", path, "error", err)
		}
	}
}

func migrateDisableNotifications() {
	prepareDisableNotificationsMigration(GlobalConfig(), GlobalConfigData()).commit()
}

// GlobalConfig returns the global configuration file path for the application.
func GlobalConfig() string {
	return globalConfigFromEnvironment(env.New())
}

func globalConfigFromEnvironment(environment env.Env) string {
	if environment == nil {
		environment = env.New()
	}
	if global := environment.Get("CRUX_GLOBAL_CONFIG"); global != "" {
		return filepath.Join(global, appName+".json")
	}
	return filepath.Join(configHomeFromEnvironment(environment), appName, appName+".json")
}

func configHomeFromEnvironment(environment env.Env) string {
	if environment == nil {
		environment = env.New()
	}
	if configHome := environment.Get("XDG_CONFIG_HOME"); configHome != "" {
		return configHome
	}
	return filepath.Join(homeDirFromEnvironment(environment), ".ai-cli")
}

func homeDirFromEnvironment(environment env.Env) string {
	if environment == nil {
		environment = env.New()
	}
	if runtime.GOOS == "windows" {
		if userProfile := environment.Get("USERPROFILE"); userProfile != "" {
			return userProfile
		}
		if homeDrive, homePath := environment.Get("HOMEDRIVE"), environment.Get("HOMEPATH"); homeDrive != "" && homePath != "" {
			return homeDrive + homePath
		}
	}
	if homeDir := environment.Get("HOME"); homeDir != "" {
		return homeDir
	}
	return home.Dir()
}

// shellConfigSibling returns the cruxrc path that sits alongside a given
// crux.json path (same directory). Used so global config locations pick up a
// shell config, not just JSON.
func shellConfigSibling(jsonPath string) string {
	return filepath.Join(filepath.Dir(jsonPath), appName+"rc")
}

// isShellConfig reports whether a config path is a shell config (cruxrc or
// the hidden .cruxrc), as opposed to a JSON config.
func isShellConfig(path string) bool {
	base := filepath.Base(path)
	return base == appName+"rc" || base == "."+appName+"rc"
}

// GlobalCacheDir returns the path to the global cache directory for the
// application.
func GlobalCacheDir() string {
	return globalCacheDirFromEnvironment(env.New())
}

func globalCacheDirFromEnvironment(environment env.Env) string {
	if environment == nil {
		environment = env.New()
	}
	if cache := environment.Get("CRUX_CACHE_DIR"); cache != "" {
		return cache
	}
	if xdgCacheHome := environment.Get("XDG_CACHE_HOME"); xdgCacheHome != "" {
		return filepath.Join(xdgCacheHome, appName)
	}
	if runtime.GOOS == "windows" {
		localAppData := cmp.Or(
			environment.Get("LOCALAPPDATA"),
			filepath.Join(environment.Get("USERPROFILE"), "AppData", "Local"),
		)
		return filepath.Join(localAppData, appName, "cache")
	}
	return filepath.Join(homeDirFromEnvironment(environment), ".cache", appName)
}

// ProjectConfigs returns list of current project configs paths.
func ProjectConfigs(cwd string) []string {
	return lookupConfigs(cwd)
}

// GlobalConfigData returns the path to the main data directory for the application.
// this config is used when the app overrides configurations instead of updating the global config.
func GlobalConfigData() string {
	return globalConfigDataFromEnvironment(appName, env.New())
}

func globalConfigDataFromEnvironment(name string, environment env.Env) string {
	if environment == nil {
		environment = env.New()
	}
	if override := environment.Get("CRUX_GLOBAL_DATA"); override != "" {
		return filepath.Join(override, name+".json")
	}
	if xdgDataHome := environment.Get("XDG_DATA_HOME"); xdgDataHome != "" {
		return filepath.Join(xdgDataHome, name, name+".json")
	}
	if runtime.GOOS == "windows" {
		localAppData := cmp.Or(
			environment.Get("LOCALAPPDATA"),
			filepath.Join(environment.Get("USERPROFILE"), "AppData", "Local"),
		)
		return filepath.Join(localAppData, name, name+".json")
	}
	return filepath.Join(homeDirFromEnvironment(environment), ".ai-cli", "data", name, name+".json")
}

// GlobalWorkspaceDir returns the path to the global server workspace
// directory. This directory acts as a meta-workspace for the server
// process, giving it a real workingDir so that config loading, scoped
// writes, and provider resolution behave identically to project
// workspaces.
func GlobalWorkspaceDir() string {
	return globalWorkspaceDirFromEnvironment(env.New())
}

func globalWorkspaceDirFromEnvironment(environment env.Env) string {
	return filepath.Dir(globalConfigDataFromEnvironment(appName, environment))
}

func assignIfNil[T any](ptr **T, val T) {
	if *ptr == nil {
		*ptr = &val
	}
}

func isInsideWorktree() bool {
	bts, err := exec.CommandContext(
		context.Background(),
		"git", "rev-parse",
		"--is-inside-work-tree",
	).CombinedOutput()
	return err == nil && strings.TrimSpace(string(bts)) == "true"
}

// worktreeRoot returns the absolute path of the git working tree root for
// dir, or the empty string if dir is not inside a working tree (bare
// repositories, missing git binary, plain directories, or any other
// failure mode). Linked worktrees and submodules each report their own
// top-level, which is what callers want when bounding lookups.
// worktreeRootCache memoizes the git worktree root per directory. The root
// is stable for the life of the process, so we avoid re-shelling out to
// "git rev-parse" on every config reload. Keyed by the requested dir; the
// value is the resolved root ("" when dir is not in a git worktree).
var worktreeRootCache sync.Map // map[string]string

func worktreeRoot(dir string) string {
	if cached, ok := worktreeRootCache.Load(dir); ok {
		return cached.(string)
	}
	root := computeWorktreeRoot(dir)
	worktreeRootCache.Store(dir, root)
	return root
}

func computeWorktreeRoot(dir string) string {
	cmd := exec.CommandContext(
		context.Background(),
		"git", "rev-parse", "--show-toplevel",
	)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	return abs
}

// projectBoundary returns the directory at which an upward configuration
// search rooted at dir should stop. It is the git working tree root when
// one can be detected, otherwise dir itself. Returning dir as a
// fallback keeps Crux from silently adopting state files placed above
// the current project.
func projectBoundary(dir string) string {
	if root := worktreeRoot(dir); root != "" {
		return root
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// GlobalSkillsDirs returns the default directories for Agent Skills.
// Skills in these directories are auto-discovered and their files can be read
// without permission prompts.
func GlobalSkillsDirs() []string {
	return globalSkillsDirsFromEnvironment(env.New())
}

func globalSkillsDirsFromEnvironment(environment env.Env) []string {
	if environment == nil {
		environment = env.New()
	}
	if skills := environment.Get("CRUX_SKILLS_DIR"); skills != "" {
		return []string{skills}
	}

	configHome := configHomeFromEnvironment(environment)
	homeDir := homeDirFromEnvironment(environment)
	paths := []string{
		filepath.Join(configHome, appName, "skills"),
		filepath.Join(configHome, "agents", "skills"),
		// Per the Agent Skills spec, scan ~/.agents/skills
		filepath.Join(homeDir, ".agents", "skills"),
		filepath.Join(homeDir, ".claude", "skills"),
	}

	// On Windows, also load from app data on top of `$HOME/.config/crux`.
	// This is here mostly for backwards compatibility.
	if runtime.GOOS == "windows" {
		appData := cmp.Or(
			environment.Get("LOCALAPPDATA"),
			filepath.Join(environment.Get("USERPROFILE"), "AppData", "Local"),
		)
		paths = append(
			paths,
			filepath.Join(appData, appName, "skills"),
			filepath.Join(appData, "agents", "skills"),
		)
	}

	return paths
}

// projectSkillSubdirs lists the conventional subdirectories where
// project-level skills are discovered. Shared across working-dir and
// git-root lookups to prevent drift when a new convention is added.
var projectSkillSubdirs = []string{
	".agents/skills",
	".crux/skills",
	".claude/skills",
	".cursor/skills",
}

// ProjectSkillsDir returns the default project directories for which Crux
// will look for skills. In addition to the working directory, it also
// checks the git working tree root so that monorepo-level skills are
// discovered when the user is inside a subdirectory.
// Working-directory paths come first so local skills take precedence
// over monorepo-level ones.
func ProjectSkillsDir(workingDir string) []string {
	dirs := make([]string, 0, len(projectSkillSubdirs)*2)
	for _, sub := range projectSkillSubdirs {
		dirs = append(dirs, filepath.Join(workingDir, sub))
	}

	// When the working directory is inside a git repository, also look at
	// the repository root so monorepo-level .agents/skills are found.
	if root := worktreeRoot(workingDir); root != "" && root != workingDir {
		for _, sub := range projectSkillSubdirs {
			dirs = append(dirs, filepath.Join(root, sub))
		}
	}

	return dirs
}

func isAppleTerminal() bool { return isAppleTerminalFromEnvironment(env.New()) }

func isAppleTerminalFromEnvironment(environment env.Env) bool {
	if environment == nil {
		environment = env.New()
	}
	return environment.Get("TERM_PROGRAM") == "Apple_Terminal"
}

// normalizeHookEvent maps user-provided event names to their canonical
// form. Matching is case-insensitive and accepts snake_case variants
// (e.g. "pre_tool_use" → "PreToolUse").
func normalizeHookEvent(name string) string {
	switch strings.ToLower(strings.ReplaceAll(name, "_", "")) {
	case "pretooluse":
		return "PreToolUse"
	default:
		return name
	}
}

// ValidateHooks normalizes event names and checks that every configured
// hook has a command and a syntactically valid matcher regex. Matcher
// compilation used for matching is owned by hooks.Runner; this function
// only validates up front so the user sees config errors at load time
// rather than on the first tool call.
func (c *Config) ValidateHooks() error {
	// Normalize event name keys.
	for event, eventHooks := range c.Hooks {
		canonical := normalizeHookEvent(event)
		if canonical != event {
			c.Hooks[canonical] = append(c.Hooks[canonical], eventHooks...)
			delete(c.Hooks, event)
		}
	}

	for event, eventHooks := range c.Hooks {
		for i, h := range eventHooks {
			if h.Command == "" {
				return fmt.Errorf("hook %s[%d]: command is required", event, i)
			}
			if h.Matcher == "" {
				continue
			}
			if _, err := regexp.Compile(h.Matcher); err != nil {
				return fmt.Errorf("hook %s[%d]: invalid matcher regex %q: %w", event, i, h.Matcher, err)
			}
		}
	}
	return nil
}
