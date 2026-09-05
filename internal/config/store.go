package config

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/redact"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

// configLockDeadline bounds how long lockConfig waits for the
// cross-process flock before giving up. A few seconds is plenty for
// honest contention; longer suggests something is wedged.
const configLockDeadline = 5 * time.Second

// refreshLockDeadline bounds how long RefreshOAuthToken waits for the
// per-provider cross-process refresh lock. It must exceed the token
// exchange HTTP timeout (30s) so that a peer mid-exchange is given time
// to finish and publish its result, which we then adopt instead of
// running our own exchange. Running our own would reuse an
// already-rotated refresh token and trip the provider's reuse detection,
// revoking the whole token family.
const refreshLockDeadline = 45 * time.Second

// credentialWriteLockDeadline bounds how long a credential write (e.g.
// storing the token from a fresh interactive login) waits for the
// per-provider refresh lock. It is deliberately shorter than
// refreshLockDeadline because a user is watching: if a peer is wedged we
// would rather write and risk a rare clobber than hang the UI.
const credentialWriteLockDeadline = 10 * time.Second

// fileSnapshot captures metadata about a config file at a point in time.
type fileSnapshot struct {
	Path    string
	Exists  bool
	Size    int64
	ModTime int64 // UnixNano
}

// RuntimeOverrides holds per-session settings that are never persisted to
// disk. They are applied on top of the loaded Config and survive only for
// the lifetime of the process (or workspace).
type RuntimeOverrides struct {
	SkipPermissionRequests bool
	// EnabledChannels lists the MCP servers opted in as channels for this
	// session (via the --channels flag). A server present in MCP config only
	// pushes channel events when it also appears here. Entries may be written
	// as "server:<name>" or as a bare "<name>".
	EnabledChannels []string
	// Models records the model choices made in this instance, whether
	// persisted or not. They are reapplied after a config reload so that a
	// selection made here always outranks whatever the shared config file
	// happens to hold — see pinPreferredModelLocked.
	Models map[SelectedModelType]SelectedModel
}

// ConfigStore is the single entry point for all config access. It owns the
// pure-data Config, runtime state (working directory, resolver, known
// providers), and persistence to both global and workspace config files.
//
// mu serialises all config file mutations (SetConfigFields,
// RemoveConfigField, RefreshOAuthToken) to prevent both in-process
// goroutine races and, together with the shared lock.File, cross-process
// races on the config file.
//
// writeMu serialises every operation that produces a new in-memory Config:
// the typed copy-on-write mutators (SetCompactMode, UpdatePreferredModel,
// ...), arbitrary config-file mutations, and ReloadFromDisk. This is what
// lets published Configs be treated as immutable: a mutator clones, mutates
// the clone, and swaps it in under writeMu rather than mutating the live
// Config in place.
type ForwardedAccount struct {
	Owner providerregistry.RegistrationOwner `json:"owner"`
	Entry accounts.Entry                     `json:"entry"`
}

type RuntimeSnapshot struct {
	config            *Config
	resolver          VariableResolver
	registry          *providerregistry.Registry
	environment       env.Env
	ephemeralAccounts map[string]ForwardedAccount
}

type RuntimeGenerationCandidate struct {
	Commit func()
	Abort  func()
}

type RuntimeGenerationPreparer func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error)

func (s RuntimeSnapshot) Config() *Config {
	return s.config
}

func (s RuntimeSnapshot) Environment() []string {
	if s.environment == nil {
		return nil
	}
	return slices.Clone(s.environment.Env())
}

func (s RuntimeSnapshot) Getenv(key string) string {
	if s.environment == nil {
		return ""
	}
	return s.environment.Get(key)
}

func (s RuntimeSnapshot) Resolve(value string) (string, error) {
	if s.resolver == nil {
		return "", fmt.Errorf("no variable resolver configured")
	}
	return s.resolver.ResolveValue(value)
}

func (s RuntimeSnapshot) ProviderRegistration(providerID string) (providerregistry.Registration, bool) {
	return s.config.providerRegistration(s.registry, providerID)
}

func (s RuntimeSnapshot) ProviderRegistrationError(providerID string) error {
	return s.config.providerRegistrationError(s.registry, providerID)
}

func (s RuntimeSnapshot) ProviderRegistrationFor(providerID string, provider ProviderConfig) (providerregistry.Registration, bool) {
	return providerRegistrationForProvider(s.registry, providerID, provider)
}

func (s RuntimeSnapshot) ProviderRegistrationErrorFor(providerID string, provider ProviderConfig) error {
	if provider.Plugin == nil || s.registry == nil {
		return nil
	}
	registration, ok := s.registry.Lookup(providerID)
	if !ok || registration.ProviderID != providerID || !pluginReferenceMatches(provider.Plugin, registration) {
		return nil
	}
	_, err := providerregistry.BindRegistrationConfiguration(registration, provider.Configuration)
	return err
}

func (s RuntimeSnapshot) ProviderPreset(providerID string) (ProviderPresetReference, bool) {
	return s.config.providerPreset(s.registry, providerID)
}

func (s RuntimeSnapshot) ProviderBehaviorRegistration(providerID string, provider ProviderConfig) (providerregistry.Registration, bool) {
	return s.ProviderRegistrationFor(providerID, provider)
}

func (s RuntimeSnapshot) ProviderOwner(providerID string) (providerregistry.RegistrationOwner, bool) {
	return providerOwnerForConfig(s.config, s.registry, providerID)
}

func (s RuntimeSnapshot) ProviderOwnerFor(providerID string, provider ProviderConfig) (providerregistry.RegistrationOwner, bool) {
	return providerOwnerForProvider(s.config, s.registry, providerID, provider)
}

func (s RuntimeSnapshot) AgentModelState() AgentModelState {
	return captureAgentModelState(s.config.Models, s.ProviderOwner)
}

func (s AgentModelState) Validate() error {
	if s.Large == nil && s.Small == nil {
		return fmt.Errorf("agent model state is required")
	}
	for modelType, selected := range map[SelectedModelType]*OwnedSelectedModel{
		SelectedModelTypeLarge: s.Large,
		SelectedModelTypeSmall: s.Small,
	} {
		if selected == nil {
			continue
		}
		if selected.Owner.ProviderID == "" {
			return fmt.Errorf("expected %s model owner is required", modelType)
		}
		if selected.Owner.ProviderID != selected.Model.Provider {
			return fmt.Errorf("expected %s model owner provider %s does not match model provider %s", modelType, selected.Owner.ProviderID, selected.Model.Provider)
		}
	}
	return nil
}

func (s RuntimeSnapshot) ValidateAgentModelState(expected AgentModelState) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, s.AgentModelState()) {
		return fmt.Errorf("agent model generation changed before runtime publication")
	}
	return nil
}

func providerOwnershipReferencesMatch(left, right ProviderConfig) bool {
	if left.Owner == nil || right.Owner == nil {
		if left.Owner != right.Owner {
			return false
		}
	} else if *left.Owner != *right.Owner {
		return false
	}
	if left.Plugin == nil || right.Plugin == nil {
		if left.Plugin != right.Plugin {
			return false
		}
	} else if *left.Plugin != *right.Plugin {
		return false
	}
	if left.Preset == nil || right.Preset == nil {
		return left.Preset == right.Preset
	}
	return *left.Preset == *right.Preset
}

func validateCompleteProviderOwner(providerID string, provider ProviderConfig) error {
	if provider.ID != "" && provider.ID != providerID {
		return fmt.Errorf("provider %q declares conflicting ID %q", providerID, provider.ID)
	}
	if provider.Owner == nil || provider.Owner.Type == "" || provider.Owner.Construction == "" {
		return fmt.Errorf("provider %q has an incomplete owner reference", providerID)
	}
	checked := provider
	owner := *provider.Owner
	checked.Owner = &owner
	if err := validateConfiguredProviderOwner(providerID, checked); err != nil {
		return err
	}
	switch owner.Type {
	case ProviderOwnerPlugin:
		if provider.Plugin.ID == "" || provider.Plugin.Version == "" {
			return fmt.Errorf("provider %q has an incomplete plugin owner reference", providerID)
		}
	case ProviderOwnerPreset:
		if provider.Preset.ID == "" || provider.Preset.Version == "" || provider.Preset.Digest == "" {
			return fmt.Errorf("provider %q has an incomplete preset owner reference", providerID)
		}
	}
	return nil
}

func (s RuntimeSnapshot) ProviderForConstruction(providerID string, provider ProviderConfig) (ProviderConfig, providerregistry.Registration, bool, error) {
	if s.config == nil || s.config.Providers == nil {
		return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %q cannot be constructed without a configuration snapshot", providerID)
	}
	if providerID == "" {
		return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("selected provider is empty")
	}
	if provider.ID != "" && provider.ID != providerID {
		return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("selected provider %q conflicts with passed provider ID %q", providerID, provider.ID)
	}
	persisted, configured := s.config.Providers.Get(providerID)
	if !configured {
		return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %q is not configured", providerID)
	}
	if err := validateCompleteProviderOwner(providerID, persisted); err != nil {
		return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %s configuration is invalid: %w", providerID, err)
	}
	if !providerOwnershipReferencesMatch(persisted, provider) {
		return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %s passed owner does not match its persisted owner", providerID)
	}
	if persisted.Disable {
		return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %s is disabled", providerID)
	}
	if _, active := providerOwnerForProvider(s.config, s.registry, providerID, persisted); !active {
		if err := s.ProviderRegistrationErrorFor(providerID, persisted); err != nil {
			return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %s configuration is invalid: %w", providerID, err)
		}
		switch persisted.Owner.Type {
		case ProviderOwnerPlugin:
			return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %s is unavailable because plugin %s version %s is not its active exact owner for construction %s", providerID, persisted.Plugin.ID, persisted.Plugin.Version, persisted.Owner.Construction)
		case ProviderOwnerPreset:
			return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %s is unavailable because preset %s version %s with its persisted digest is not its active exact owner", providerID, persisted.Preset.ID, persisted.Preset.Version)
		case ProviderOwnerCore:
			return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %s is unavailable because its core construction %s is not active for the selected provider profile", providerID, persisted.Owner.Construction)
		default:
			return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %s exact owner is unavailable or mismatched", providerID)
		}
	}
	persisted = cloneProviderConfig(persisted)
	persisted.ID = providerID
	switch persisted.Owner.Type {
	case ProviderOwnerCore, ProviderOwnerPlugin:
		registration, registered := providerRegistrationForProvider(s.registry, providerID, persisted)
		if !registered {
			return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %s exact registered owner is unavailable or mismatched", providerID)
		}
		return persisted, registration, true, nil
	case ProviderOwnerPreset, ProviderOwnerCustom:
		return persisted, providerregistry.Registration{}, false, nil
	default:
		return ProviderConfig{}, providerregistry.Registration{}, false, fmt.Errorf("provider %s uses unsupported owner type %q", providerID, persisted.Owner.Type)
	}
}

func (s RuntimeSnapshot) EphemeralAccount(expected providerregistry.RegistrationOwner) (*accounts.Entry, bool) {
	if expected.ProviderID == "" || expected.AccountNamespace == "" {
		return nil, false
	}
	forwarded, ok := s.ephemeralAccounts[expected.AccountNamespace]
	if !ok || forwarded.Owner != expected {
		return nil, false
	}
	registration, ok := s.ProviderRegistration(expected.ProviderID)
	if !ok || !expected.Matches(registration) {
		return nil, false
	}
	entry := forwarded.Entry
	entry.Raw = slices.Clone(entry.Raw)
	return &entry, true
}

type ConfigStore struct {
	config                   *Config
	ephemeralAccounts        map[string]ForwardedAccount
	ephemeralProviderConfigs map[string]ProviderConfig
	ephemeralProviders       map[string]struct{}
	workingDir               string
	resolver                 VariableResolver
	baseEnvironment          env.Env
	effectiveEnvironment     env.Env
	appliedEnvironment       map[string]string
	publishProcessState      bool
	globalDataPath           string   // ~/.ai-cli/data/crux/crux.json
	workspacePath            string   // .crux/crux.json
	loadedPaths              []string // config files that were successfully loaded
	knownProviders           []catalog.Provider
	providerRegistry         *providerregistry.Registry
	overrides                RuntimeOverrides
	setEnvironment           func(string, string) error
	unsetEnvironment         func(string) error
	runtimePreparer          RuntimeGenerationPreparer
	writeFields              func(Scope, map[string]any) error
	trackedConfigPaths       []string                // unique, normalized config file paths
	snapshots                map[string]fileSnapshot // path -> snapshot at last capture

	// configMu guards the config pointer field against concurrent
	// readers (Config) and the writeMu-serialised swap (setConfig). It
	// protects the pointer word only; the pointed-to Config is treated
	// as immutable once published, since both reloads and typed mutators
	// build a fresh Config rather than mutating the live one.
	configMu sync.RWMutex

	mu      sync.Mutex   // serialises config file writes
	writeMu sync.RWMutex // serialises in-memory config production (mutators + reload); RLock for readers

	// refreshSF collapses concurrent in-process OAuth refreshes for the
	// same provider into a single attempt. Combined with the per-provider
	// cross-process refresh lock, it ensures only one token exchange runs
	// at a time. See RefreshOAuthToken.
	refreshSF singleflight.Group

	// exchangeToken performs the provider-specific OAuth token exchange.
	// It is a field so tests can substitute a fake exchange without making
	// real network calls. Production code leaves it nil, and exchange falls
	// back to the real provider clients.
	exchangeToken func(ctx context.Context, providerID, refreshToken string) (*oauth.Token, error)

	// authSignalMu guards authSignals, which maps exact owners to
	// channels that WaitForTokenChange blocks on. SignalAuthComplete
	// closes the channel to unblock waiters; a new channel is created
	// on the next wait.
	authSignalMu sync.Mutex
	authSignals  map[providerregistry.RegistrationOwner]chan struct{}
}

// Config returns the pure-data config struct (read-only after load).
//
// The pointer read is guarded by configMu so it can never tear against
// the reload swap in reloadFromDiskLocked. Reloads build a brand-new
// Config and swap it in rather than mutating the live one, so holding the
// returned pointer stays safe even across a concurrent reload — the reader
// keeps reading its (now immutable) snapshot.
func (s *ConfigStore) Config() *Config {
	providerStateMu.RLock()
	defer providerStateMu.RUnlock()
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

func (s *ConfigStore) RuntimeSnapshot() RuntimeSnapshot {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
}

func (s *ConfigStore) Environment() []string {
	return s.RuntimeSnapshot().Environment()
}

func (s *ConfigStore) WithRuntimeSnapshot(build func(RuntimeSnapshot) error) error {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	s.configMu.RLock()
	snapshot := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.RUnlock()
	return build(snapshot)
}

func (s *ConfigStore) runtimeSnapshotLocked(cfg *Config, resolver VariableResolver, registry *providerregistry.Registry, environment env.Env) RuntimeSnapshot {
	snapshot := RuntimeSnapshot{
		config:            cfg,
		resolver:          resolver,
		environment:       cloneEnvironment(environment),
		ephemeralAccounts: make(map[string]ForwardedAccount, len(s.ephemeralAccounts)),
	}
	for namespace, forwarded := range s.ephemeralAccounts {
		forwarded.Entry.Raw = slices.Clone(forwarded.Entry.Raw)
		snapshot.ephemeralAccounts[namespace] = forwarded
	}
	if registry != nil {
		snapshot.registry = registry.Clone()
	}
	return snapshot
}

func (s *ConfigStore) SetRuntimeGenerationPreparer(preparer RuntimeGenerationPreparer) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.runtimePreparer = preparer
}

func (s *ConfigStore) prepareRuntimeGeneration(ctx context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
	if s.runtimePreparer == nil {
		return RuntimeGenerationCandidate{}, nil
	}
	candidate, err := s.runtimePreparer(ctx, snapshot)
	if err != nil {
		return RuntimeGenerationCandidate{}, err
	}
	if candidate.Commit == nil || candidate.Abort == nil {
		return RuntimeGenerationCandidate{}, errors.New("runtime generation preparer returned an incomplete candidate")
	}
	return candidate, nil
}

func (s *ConfigStore) applyProcessEnvironment(base env.Env, previous, current map[string]string) error {
	setenv := s.setEnvironment
	if setenv == nil {
		setenv = os.Setenv
	}
	unsetenv := s.unsetEnvironment
	if unsetenv == nil {
		unsetenv = os.Unsetenv
	}
	return applyEnvironmentWith(base, previous, current, setenv, unsetenv)
}

// setConfig atomically swaps the active config pointer under configMu.
// Used by the reload path; in-place field mutators leave the pointer
// untouched and run under mu instead.
func (s *ConfigStore) setConfig(cfg *Config) {
	registerConfigSecrets(cfg)
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config = cfg
}

func validateForwardedAccounts(cfg *Config, forwardedAccounts map[string]ForwardedAccount) error {
	for namespace, forwarded := range forwardedAccounts {
		if namespace == "" {
			return fmt.Errorf("forwarded account namespace is empty")
		}
		if forwarded.Owner.ProviderID == "" || forwarded.Owner.AccountNamespace == "" {
			return fmt.Errorf("forwarded account %q is missing its exact provider owner", namespace)
		}
		if forwarded.Owner.AccountNamespace != namespace {
			return fmt.Errorf("forwarded account %q declares conflicting namespace %q", namespace, forwarded.Owner.AccountNamespace)
		}
		registration, ok := cfg.ProviderRegistration(forwarded.Owner.ProviderID)
		if !ok || !forwarded.Owner.Matches(registration) {
			return fmt.Errorf("forwarded account %q does not match the active exact owner for provider %s", namespace, forwarded.Owner.ProviderID)
		}
		if registration.OAuth == nil {
			return fmt.Errorf("forwarded account %q exact owner for provider %s does not support OAuth accounts", namespace, forwarded.Owner.ProviderID)
		}
	}
	return nil
}

func (s *ConfigStore) ApplyEphemeralProviderState(providers map[string]ProviderConfig, forwardedAccounts map[string]ForwardedAccount) error {
	if len(providers) == 0 && len(forwardedAccounts) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	current := s.Config()
	if err := validateForwardedProviderState(current, providers); err != nil {
		return fmt.Errorf("validate ephemeral provider state: %w", err)
	}
	overlay, err := json.Marshal(struct {
		Providers map[string]ProviderConfig `json:"providers,omitempty"`
	}{Providers: providers})
	if err != nil {
		return fmt.Errorf("encode ephemeral provider state: %w", err)
	}
	merged, err := loadFromBytes([][]byte{mustMarshalConfig(current), overlay})
	if err != nil {
		return fmt.Errorf("apply ephemeral provider state: %w", err)
	}
	baseEnvironment := s.baseEnvironment
	if baseEnvironment == nil {
		baseEnvironment = snapshotEnvironment()
	}
	if err := merged.setDefaultsFromEnvironment(s.workingDir, current.Options.DataDirectory, baseEnvironment); err != nil {
		return fmt.Errorf("apply ephemeral provider defaults: %w", err)
	}
	merged.explicitModels = maps.Clone(current.explicitModels)
	if current.providerScan != nil {
		merged.bindProviderScan(*current.providerScan)
	}
	if err := validateForwardedAccounts(merged, forwardedAccounts); err != nil {
		return fmt.Errorf("validate ephemeral account state: %w", err)
	}
	merged.SetupAgents()

	registerConfigSecrets(merged)
	clonedAccounts := make(map[string]ForwardedAccount, len(forwardedAccounts))
	for namespace, forwarded := range forwardedAccounts {
		registerAccountSecrets(forwarded.Entry)
		forwarded.Entry.Raw = slices.Clone(forwarded.Entry.Raw)
		clonedAccounts[namespace] = forwarded
	}
	ephemeralProviders := make(map[string]struct{}, len(providers)+len(forwardedAccounts))
	for id := range providers {
		ephemeralProviders[id] = struct{}{}
	}
	for _, forwarded := range forwardedAccounts {
		ephemeralProviders[forwarded.Owner.ProviderID] = struct{}{}
	}

	s.configMu.Lock()
	s.config = merged
	s.ephemeralAccounts = clonedAccounts
	s.ephemeralProviderConfigs = maps.Clone(providers)
	s.ephemeralProviders = ephemeralProviders
	s.configMu.Unlock()
	return nil
}

func (s *ConfigStore) EphemeralAccount(expected providerregistry.RegistrationOwner) (*accounts.Entry, bool) {
	return s.RuntimeSnapshot().EphemeralAccount(expected)
}

func (s *ConfigStore) isEphemeralProvider(providerID string) bool {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	_, ok := s.ephemeralProviders[providerID]
	return ok
}

func (s *ConfigStore) ephemeralProviderSnapshot() map[string]ProviderConfig {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return maps.Clone(s.ephemeralProviderConfigs)
}

func (s *ConfigStore) applyEphemeralToken(token *oauth.Token, expected providerregistry.RegistrationOwner) error {
	if token == nil {
		return fmt.Errorf("OAuth token is nil")
	}
	if expected.ProviderID == "" {
		return fmt.Errorf("forwarded OAuth owner provider is empty")
	}
	redact.Register(token.AccessToken, token.RefreshToken)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cfg := s.Config()
	registration, err := exactOAuthRegistrationFor(cfg, s.providerRegistry, expected.ProviderID)
	if err != nil {
		return err
	}
	if !expected.Matches(registration) {
		return fmt.Errorf("OAuth owner for provider %s changed before the token could be applied", expected.ProviderID)
	}
	var forwarded ForwardedAccount
	var hasForwardedAccount bool
	if expected.AccountNamespace != "" {
		s.configMu.RLock()
		forwarded, hasForwardedAccount = s.ephemeralAccounts[expected.AccountNamespace]
		s.configMu.RUnlock()
		if hasForwardedAccount && forwarded.Owner != expected {
			return fmt.Errorf("forwarded account owner for provider %s changed before the token could be applied", expected.ProviderID)
		}
	}
	providerConfig, ok := cfg.Providers.Get(expected.ProviderID)
	if !ok {
		return fmt.Errorf("provider %s not found", expected.ProviderID)
	}
	applyOAuthTokenToProvider(&providerConfig, token, registration)
	next := cfg.cloneForWrite()
	next.Providers.Set(expected.ProviderID, providerConfig)
	registerConfigSecrets(next)

	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config = next
	if s.ephemeralProviderConfigs == nil {
		s.ephemeralProviderConfigs = make(map[string]ProviderConfig)
	}
	s.ephemeralProviderConfigs[expected.ProviderID] = providerConfig
	if !hasForwardedAccount {
		return nil
	}
	forwarded.Entry = accounts.FromToken(forwarded.Entry.ID, forwarded.Entry.DisplayName, token, &forwarded.Entry)
	registerAccountSecrets(forwarded.Entry)
	s.ephemeralAccounts[expected.AccountNamespace] = forwarded
	return nil
}

// WorkingDir returns the current working directory.
func (s *ConfigStore) WorkingDir() string {
	return s.workingDir
}

// Resolver returns the variable resolver.
func (s *ConfigStore) Resolver() VariableResolver {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return s.resolver
}

// Resolve resolves a variable reference using the configured resolver.
func (s *ConfigStore) Resolve(key string) (string, error) {
	s.writeMu.RLock()
	r := s.resolver
	s.writeMu.RUnlock()
	if r == nil {
		return "", fmt.Errorf("no variable resolver configured")
	}
	return r.ResolveValue(key)
}

// KnownProviders returns the list of known providers.
func (s *ConfigStore) KnownProviders() []catalog.Provider {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return cloneProviderCatalog(s.knownProviders)
}

// ProviderRegistration returns the immutable capability registration selected
// for a logical provider in this configuration generation.
func (s *ConfigStore) ProviderRegistration(providerID string) (providerregistry.Registration, bool) {
	s.writeMu.RLock()
	registry := s.providerRegistry
	cfg := s.config
	s.writeMu.RUnlock()
	return cfg.providerRegistration(registry, providerID)
}

func providerOwnerForProvider(cfg *Config, registry *providerregistry.Registry, providerID string, provider ProviderConfig) (providerregistry.RegistrationOwner, bool) {
	if cfg == nil || providerID == "" || provider.ID != "" && provider.ID != providerID ||
		provider.Owner == nil || provider.Owner.Type == "" || provider.Owner.Construction == "" {
		return providerregistry.RegistrationOwner{}, false
	}
	switch provider.Owner.Type {
	case ProviderOwnerPlugin, ProviderOwnerCore:
		registration, ok := providerRegistrationForProvider(registry, providerID, provider)
		if !ok {
			return providerregistry.RegistrationOwner{}, false
		}
		return registration.Owner(), true
	case ProviderOwnerPreset:
		checked := provider
		owner := *provider.Owner
		checked.Owner = &owner
		if err := validateConfiguredProviderOwner(providerID, checked); err != nil {
			return providerregistry.RegistrationOwner{}, false
		}
		preset, active := activeProviderPresetForRegistry(cfg, registry, providerID)
		if !active || !providerPresetReferenceMatches(providerID, provider.Preset, preset) {
			return providerregistry.RegistrationOwner{}, false
		}
		return providerregistry.RegistrationOwner{
			ProviderID:    providerID,
			HasPreset:     true,
			PresetID:      preset.ID,
			PresetVersion: preset.Version,
			PresetDigest:  preset.Digest,
		}, true
	case ProviderOwnerCustom:
		checked := provider
		owner := *provider.Owner
		checked.Owner = &owner
		if err := validateConfiguredProviderOwner(providerID, checked); err != nil ||
			owner.Construction != providerregistry.ConstructionOpenAICompat || owner.CompatibilityAdapter != "" ||
			provider.OAuthToken != nil && provider.Type != catalog.TypeOpenAICompat {
			return providerregistry.RegistrationOwner{}, false
		}
		return providerregistry.RegistrationOwner{ProviderID: providerID}, true
	default:
		return providerregistry.RegistrationOwner{}, false
	}
}

func providerOwnerForConfig(cfg *Config, registry *providerregistry.Registry, providerID string) (providerregistry.RegistrationOwner, bool) {
	if cfg == nil || providerID == "" {
		return providerregistry.RegistrationOwner{}, false
	}
	provider, configured := ProviderConfig{}, false
	if cfg.Providers != nil {
		provider, configured = cfg.Providers.Get(providerID)
	}
	if configured && provider.Owner != nil {
		return providerOwnerForProvider(cfg, registry, providerID, provider)
	}
	if configured {
		switch {
		case provider.Plugin != nil:
			if registry == nil {
				return providerregistry.RegistrationOwner{}, false
			}
			registration, ok := registry.Lookup(providerID)
			if !ok || registration.ProviderID != providerID || !pluginReferenceMatches(provider.Plugin, registration) {
				return providerregistry.RegistrationOwner{}, false
			}
			legacy := provider
			legacy.Owner = providerOwnerReferenceForRegistration(registration)
			return providerOwnerForProvider(cfg, registry, providerID, legacy)
		case provider.Preset != nil:
			preset, active := cfg.providerPreset(registry, providerID)
			if !active {
				return providerregistry.RegistrationOwner{}, false
			}
			return providerregistry.RegistrationOwner{
				ProviderID:    providerID,
				HasPreset:     true,
				PresetID:      preset.ID,
				PresetVersion: preset.Version,
				PresetDigest:  preset.Digest,
			}, true
		default:
			if construction, core := coreProviderConstruction(providerID); core {
				legacy := provider
				legacy.Owner = &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: construction}
				return providerOwnerForProvider(cfg, registry, providerID, legacy)
			}
			return providerregistry.RegistrationOwner{ProviderID: providerID}, true
		}
	}
	if registration, ok := cfg.providerRegistration(registry, providerID); ok {
		return registration.Owner(), true
	}
	if preset, active := cfg.providerPreset(registry, providerID); active {
		return providerregistry.RegistrationOwner{
			ProviderID:    providerID,
			HasPreset:     true,
			PresetID:      preset.ID,
			PresetVersion: preset.Version,
			PresetDigest:  preset.Digest,
		}, true
	}
	return providerregistry.RegistrationOwner{}, false
}

func (s *ConfigStore) validateRegistrationOwnerLocked(cfg *Config, expected providerregistry.RegistrationOwner) error {
	if expected.ProviderID == "" {
		return fmt.Errorf("registration owner provider is empty")
	}
	current, ok := providerOwnerForConfig(cfg, s.providerRegistry, expected.ProviderID)
	if !ok || current != expected {
		return fmt.Errorf("registration owner for provider %s changed", expected.ProviderID)
	}
	return nil
}

func (s *ConfigStore) ValidateRegistrationOwner(expected providerregistry.RegistrationOwner) error {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return s.validateRegistrationOwnerLocked(s.Config(), expected)
}

func (s *ConfigStore) validateActiveProviderOwnerLocked(cfg *Config, expected providerregistry.RegistrationOwner) error {
	if expected.ProviderID == "" {
		return fmt.Errorf("provider owner provider is empty")
	}
	if cfg == nil || cfg.Providers == nil {
		return fmt.Errorf("active owner for provider %s changed", expected.ProviderID)
	}
	provider, ok := cfg.Providers.Get(expected.ProviderID)
	if !ok || provider.Disable {
		return fmt.Errorf("active owner for provider %s changed", expected.ProviderID)
	}
	current, ok := providerOwnerForConfig(cfg, s.providerRegistry, expected.ProviderID)
	if !ok || current != expected {
		return fmt.Errorf("active owner for provider %s changed", expected.ProviderID)
	}
	return nil
}

func (s *ConfigStore) ValidateActiveProviderOwner(expected providerregistry.RegistrationOwner) error {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return s.validateActiveProviderOwnerLocked(s.Config(), expected)
}

func (s *ConfigStore) SetResolvedProviderAPIKey(expected providerregistry.RegistrationOwner, template, apiKey string) error {
	if expected.ProviderID == "" {
		return fmt.Errorf("registration owner provider is empty")
	}
	redact.Register(apiKey)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cfg := s.Config()
	registration, ok := cfg.providerRegistration(s.providerRegistry, expected.ProviderID)
	if !ok || !expected.Matches(registration) {
		return fmt.Errorf("registration owner for provider %s changed before the API key could be applied", expected.ProviderID)
	}
	provider, ok := cfg.Providers.Get(expected.ProviderID)
	if !ok {
		return fmt.Errorf("provider %s not found", expected.ProviderID)
	}
	if provider.APIKeyTemplate != template {
		return fmt.Errorf("API key template for provider %s changed before the resolved key could be applied", expected.ProviderID)
	}
	provider.APIKey = apiKey
	next := cfg.cloneForWrite()
	next.Providers.Set(expected.ProviderID, provider)
	s.setConfig(next)
	return nil
}

func (s *ConfigStore) exactOAuthRegistration(cfg *Config, providerID string) (providerregistry.Registration, error) {
	s.writeMu.RLock()
	registry := s.providerRegistry
	s.writeMu.RUnlock()
	return exactOAuthRegistrationFor(cfg, registry, providerID)
}

func exactOAuthRegistrationFor(cfg *Config, registry *providerregistry.Registry, providerID string) (providerregistry.Registration, error) {
	registration, ok := cfg.providerRegistration(registry, providerID)
	if !ok || registration.OAuth == nil {
		return providerregistry.Registration{}, fmt.Errorf("OAuth owner for provider %s is not active", providerID)
	}
	return registration, nil
}

func applyOAuthTokenToProvider(providerConfig *ProviderConfig, token *oauth.Token, registration providerregistry.Registration) {
	providerConfig.OAuthToken = token
	providerConfig.APIKey = token.AccessToken
	if registration.Construction == providerregistry.ConstructionCopilot {
		if providerConfig.ExtraHeaders == nil {
			providerConfig.ExtraHeaders = make(map[string]string)
		}
		providerConfig.SetupGitHubCopilot()
	}
}

// SetupAgents configures the coder and task agents on the config.
func (s *ConfigStore) SetupAgents() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	next := s.Config().cloneForWrite()
	next.SetupAgents()
	s.setConfig(next)
}

// Overrides returns the runtime overrides for this store.
func (s *ConfigStore) Overrides() RuntimeOverrides {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return cloneRuntimeOverrides(s.overrides)
}

func (s *ConfigStore) SetRuntimeOverrides(skipPermissionRequests bool, enabledChannels []string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.overrides.SkipPermissionRequests = skipPermissionRequests
	s.overrides.EnabledChannels = slices.Clone(enabledChannels)
}

func cloneRuntimeOverrides(overrides RuntimeOverrides) RuntimeOverrides {
	overrides.EnabledChannels = slices.Clone(overrides.EnabledChannels)
	overrides.Models = maps.Clone(overrides.Models)
	for modelType, model := range overrides.Models {
		overrides.Models[modelType] = cloneSelectedModel(model)
	}
	return overrides
}

func (c *Config) AgentModelState() AgentModelState {
	if c == nil {
		return AgentModelState{}
	}
	return captureAgentModelState(c.Models, c.ProviderOwner)
}

func captureAgentModelState(models map[SelectedModelType]SelectedModel, ownerFor func(string) (providerregistry.RegistrationOwner, bool)) AgentModelState {
	state := AgentModelState{}
	for modelType, destination := range map[SelectedModelType]**OwnedSelectedModel{
		SelectedModelTypeLarge: &state.Large,
		SelectedModelTypeSmall: &state.Small,
	} {
		model, ok := models[modelType]
		if !ok {
			continue
		}
		owner, _ := ownerFor(model.Provider)
		*destination = &OwnedSelectedModel{Model: cloneSelectedModel(model), Owner: owner}
	}
	return state
}

func cloneSelectedModel(model SelectedModel) SelectedModel {
	model.Temperature = clonePointer(model.Temperature)
	model.TopP = clonePointer(model.TopP)
	model.TopK = clonePointer(model.TopK)
	model.FrequencyPenalty = clonePointer(model.FrequencyPenalty)
	model.PresencePenalty = clonePointer(model.PresencePenalty)
	model.ProviderOptions = cloneProviderOptions(model.ProviderOptions)
	return model
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneProviderOptions(options map[string]any) map[string]any {
	if options == nil {
		return nil
	}
	clone := make(map[string]any, len(options))
	for key, value := range options {
		clone[key] = cloneProviderOptionValue(value)
	}
	return clone
}

func cloneProviderOptionValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneProviderOptionReflect(reflect.ValueOf(value)).Interface()
}

func cloneProviderOptionReflect(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		clone := reflect.New(value.Type()).Elem()
		clone.Set(cloneProviderOptionReflect(value.Elem()))
		return clone
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		clone := reflect.MakeMapWithSize(value.Type(), value.Len())
		for _, key := range value.MapKeys() {
			clone.SetMapIndex(key, cloneProviderOptionReflect(value.MapIndex(key)))
		}
		return clone
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		clone := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := range value.Len() {
			clone.Index(index).Set(cloneProviderOptionReflect(value.Index(index)))
		}
		return clone
	case reflect.Array:
		clone := reflect.New(value.Type()).Elem()
		for index := range value.Len() {
			clone.Index(index).Set(cloneProviderOptionReflect(value.Index(index)))
		}
		return clone
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		clone := reflect.New(value.Type().Elem())
		clone.Elem().Set(cloneProviderOptionReflect(value.Elem()))
		return clone
	default:
		return value
	}
}

// LoadedPaths returns the config file paths that were successfully loaded.
func (s *ConfigStore) LoadedPaths() []string {
	s.writeMu.RLock()
	defer s.writeMu.RUnlock()
	return slices.Clone(s.loadedPaths)
}

// lockConfig acquires both the in-process mutex and a cross-process flock
// on the config file for the given scope. Callers that need to do I/O
// between reading and writing (e.g. an HTTP token exchange) must use
// lockConfig explicitly rather than atomicWrite.
//
// The returned release function drops both locks. Callers must call it
// as soon as the file access is complete — no I/O should be performed
// while the lock is held.
func (s *ConfigStore) lockConfig(scope Scope) (func(), error) {
	s.mu.Lock()
	path, err := s.configPath(scope)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), configLockDeadline)
	defer cancel()
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("acquire config lock: %w", err)
	}
	return func() {
		release()
		s.mu.Unlock()
	}, nil
}

// atomicWrite handles the lock-read-transform-write-unlock cycle for
// config file mutations. The fn callback receives the current file
// contents (raw bytes, or {} if the file is missing) and must return the
// new contents. fn must be pure — no I/O, no network calls.
func (s *ConfigStore) atomicWrite(scope Scope, fn func(current []byte) ([]byte, error)) error {
	unlock, err := s.lockConfig(scope)
	if err != nil {
		return err
	}
	defer unlock()

	path, err := s.configPath(scope)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			data = []byte("{}")
		} else {
			return fmt.Errorf("read config file: %w", err)
		}
	}

	newData, err := fn(data)
	if err != nil {
		return err
	}

	return atomicWriteFile(path, newData, 0o600)
}

// configPath returns the file path for the given scope.
func (s *ConfigStore) configPath(scope Scope) (string, error) {
	switch scope {
	case ScopeWorkspace:
		if s.workspacePath == "" {
			return "", ErrNoWorkspaceConfig
		}
		return s.workspacePath, nil
	default:
		return s.globalDataPath, nil
	}
}

// HasConfigField checks whether a key exists in the config file for the given
// scope.
func (s *ConfigStore) HasConfigField(scope Scope, key string) bool {
	path, err := s.configPath(scope)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return gjson.Get(string(data), key).Exists()
}

// SetConfigField sets a key/value pair in the config file for the given scope.
// After a successful write, it automatically reloads config to keep in-memory
// state fresh.
func (s *ConfigStore) SetConfigField(scope Scope, key string, value any) error {
	return s.SetConfigFields(scope, map[string]any{key: value})
}

// SetConfigFields sets multiple key/value pairs in the config file for the
// given scope in a single write, then reloads in-memory state from disk.
//
// Use this for arbitrary external edits where the in-memory effect of the
// change is not known ahead of time. The typed mutators (which know exactly
// what changed) go through update instead and skip the reload.
//
// The write is protected by an in-process mutex and a cross-process flock
// to prevent races between concurrent writers in different processes.
func (s *ConfigStore) SetConfigFields(scope Scope, kv map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.configPath(scope); err != nil {
		return err
	}
	if s.workingDir == "" {
		return fmt.Errorf("cannot publish config fields without a working directory")
	}
	if err := s.writeConfigFields(scope, kv); err != nil {
		return err
	}
	if err := s.reloadFromDiskLocked(context.Background()); err != nil {
		return fmt.Errorf("config file updated but failed to publish in-memory state: %w", err)
	}
	return nil
}

// writeConfigFields persists key/value pairs to the config file. It does not
// touch in-memory config state or the staleness snapshot: callers either
// reload (SetConfigFields, whose reload recaptures the snapshot) or have
// already published an updated clone and capture the snapshot themselves
// (update). Both of those run under writeMu, which is what keeps the
// snapshot map free of concurrent writers.
func (s *ConfigStore) writeConfigFields(scope Scope, kv map[string]any) error {
	// Sort keys for deterministic output regardless of map iteration
	// order. This also ensures consistent results when callers pass
	// overlapping JSONPath keys (e.g. "a" and "a.b").
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	return s.atomicWrite(scope, func(data []byte) ([]byte, error) {
		v := string(data)
		for _, key := range keys {
			var sErr error
			if v, sErr = sjson.Set(v, key, kv[key]); sErr != nil {
				return nil, fmt.Errorf("failed to set config field %s: %w", key, sErr)
			}
		}
		return []byte(v), nil
	})
}

func (s *ConfigStore) persistConfigFields(scope Scope, fields map[string]any) error {
	if s.writeFields != nil {
		return s.writeFields(scope, fields)
	}
	return s.writeConfigFields(scope, fields)
}

// mutateInMemory applies a copy-on-write change to the config without
// persisting. Under writeMu it clones the live config, lets mutate edit the
// clone, and publishes it. This is the single primitive every in-memory
// config change goes through, so a published Config is never mutated in
// place and readers always see a consistent snapshot.
func (s *ConfigStore) mutateInMemory(mutate func(*Config)) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	nc := s.Config().cloneForWrite()
	mutate(nc)
	s.setConfig(nc)
}

// update applies a copy-on-write change and persists the reported fields.
// mutate edits the clone and returns the JSON-path fields to write to disk;
// because the clone already reflects the change, no reload is needed.
// Returning an empty map publishes the clone without a disk write.
func (s *ConfigStore) update(scope Scope, mutate func(*Config) map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.updateLocked(scope, mutate)
}

// updateLocked is the lock-free core of update. Caller must hold writeMu.
func (s *ConfigStore) updateLocked(scope Scope, mutate func(*Config) map[string]any) error {
	nc := s.Config().cloneForWrite()
	fields := mutate(nc)
	if len(fields) > 0 {
		if err := s.persistConfigFields(scope, fields); err != nil {
			return err
		}
		// Refresh the staleness snapshot so the file watcher does not treat
		// our own write as an external change. Safe to touch the snapshot map
		// here because we hold writeMu.
		if path, err := s.configPath(scope); err == nil {
			s.captureStalenessSnapshot(append(slices.Clone(s.loadedPaths), path))
		}
	}
	s.setConfig(nc)
	return nil
}

// OverridePreferredModel sets the preferred model for the given type in
// memory only, without persisting. It is for per-run overrides (such as the
// non-interactive --model flags) that must not be written to the user's
// config file.
func (s *ConfigStore) OverridePreferredModel(modelType SelectedModelType, model SelectedModel) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.overridePreferredModelLocked(modelType, model)
}

func (s *ConfigStore) OverridePreferredModelForOwner(modelType SelectedModelType, model SelectedModel, expected providerregistry.RegistrationOwner) error {
	if model.Provider != expected.ProviderID {
		return fmt.Errorf("model provider %s does not match initiating owner provider %s", model.Provider, expected.ProviderID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.validateActiveProviderOwnerLocked(s.Config(), expected); err != nil {
		return err
	}
	return s.overridePreferredModelLocked(modelType, model)
}

func (s *ConfigStore) overridePreferredModelLocked(modelType SelectedModelType, model SelectedModel) error {
	current := s.Config()
	if !current.IsModelAvailable(model.Provider, model.Model) {
		return fmt.Errorf("model %q for provider %q is not available", model.Model, model.Provider)
	}
	model = cloneSelectedModel(model)
	small, updateSmall, err := s.implicitSmallModel(current, modelType, model)
	if err != nil {
		return err
	}

	nc := current.cloneForWrite()
	if nc.Models == nil {
		nc.Models = make(map[SelectedModelType]SelectedModel)
	}
	nc.Models[modelType] = model
	nc.markModelExplicit(modelType)
	if updateSmall {
		nc.Models[SelectedModelTypeSmall] = small
	}
	s.pinPreferredModelLocked(modelType, model)
	s.setConfig(nc)
	return nil
}

// pinPreferredModelLocked records a model choice made in this instance so
// that a later config reload cannot replace it with a choice made
// somewhere else. Several Crux instances share one global config file, so
// a reload triggered by an unrelated write (a token refresh, say) would
// otherwise import whichever model a sibling instance last selected and
// switch models out from under the user mid-session.
//
// Caller must hold writeMu.
func (s *ConfigStore) pinPreferredModelLocked(modelType SelectedModelType, model SelectedModel) {
	if s.overrides.Models == nil {
		s.overrides.Models = make(map[SelectedModelType]SelectedModel)
	}
	s.overrides.Models[modelType] = cloneSelectedModel(model)
}

// RemoveConfigField removes a key from the config file for the given scope.
// After a successful write, it automatically reloads config to keep in-memory
// state fresh.
//
// The write is protected by an in-process mutex and a cross-process flock.
func (s *ConfigStore) RemoveConfigField(scope Scope, key string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.configPath(scope); err != nil {
		return err
	}
	if s.workingDir == "" {
		return fmt.Errorf("cannot publish config field removal without a working directory")
	}
	if err := s.atomicWrite(scope, func(data []byte) ([]byte, error) {
		value, deleteErr := sjson.Delete(string(data), key)
		if deleteErr != nil {
			return nil, fmt.Errorf("failed to delete config field %s: %w", key, deleteErr)
		}
		return []byte(value), nil
	}); err != nil {
		return err
	}
	if err := s.reloadFromDiskLocked(context.Background()); err != nil {
		return fmt.Errorf("config file updated but failed to publish in-memory state: %w", err)
	}
	return nil
}

// UpdatePreferredModel updates the preferred model for the given type and
// persists it to the config file at the given scope. The selected model and
// the recent-models list are written together in a single config write.
//
// The write skips the full disk reparse/reload (which would rebuild the
// provider catalog and agents on every model switch and dominate selection
// latency); agents are refreshed separately by the caller (see
// UpdateAgentModel).
func (s *ConfigStore) UpdatePreferredModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.updatePreferredModelLocked(scope, modelType, model)
}

func (s *ConfigStore) UpdatePreferredModelForOwner(scope Scope, modelType SelectedModelType, model SelectedModel, expected providerregistry.RegistrationOwner) (AgentModelState, error) {
	if model.Provider != expected.ProviderID {
		return AgentModelState{}, fmt.Errorf("model provider %s does not match initiating owner provider %s", model.Provider, expected.ProviderID)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.validateActiveProviderOwnerLocked(s.Config(), expected); err != nil {
		return AgentModelState{}, err
	}
	if err := s.updatePreferredModelLocked(scope, modelType, model); err != nil {
		return AgentModelState{}, err
	}
	cfg := s.Config()
	return captureAgentModelState(cfg.Models, func(providerID string) (providerregistry.RegistrationOwner, bool) {
		return providerOwnerForConfig(cfg, s.providerRegistry, providerID)
	}), nil
}

func (s *ConfigStore) updatePreferredModelLocked(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	current := s.Config()
	if !current.IsModelAvailable(model.Provider, model.Model) {
		return fmt.Errorf("model %q for provider %q is not available", model.Model, model.Provider)
	}
	small, updateSmall, err := s.implicitSmallModel(current, modelType, model)
	if err != nil {
		return err
	}
	if err := s.updateLocked(scope, func(c *Config) map[string]any {
		fields := s.updatePreferredModelFields(c, modelType, model)
		if updateSmall {
			c.Models[SelectedModelTypeSmall] = small
		}
		return fields
	}); err != nil {
		return err
	}
	s.pinPreferredModelLocked(modelType, model)
	return nil
}

func (s *ConfigStore) implicitSmallModel(c *Config, modelType SelectedModelType, model SelectedModel) (SelectedModel, bool, error) {
	if modelType != SelectedModelTypeLarge || c.modelExplicit(SelectedModelTypeSmall) {
		return SelectedModel{}, false, nil
	}
	small, err := c.defaultModelForProvider(model.Provider, SelectedModelTypeSmall, s.knownProviders)
	if err != nil {
		return SelectedModel{}, false, fmt.Errorf("resolve default small model for provider %s: %w", model.Provider, err)
	}
	return small, true, nil
}

// updatePreferredModelFields builds the fields map for persisting a preferred
// model change. Shared between UpdatePreferredModel and direct updateLocked
// callers (e.g. Load). Caller must hold writeMu.
func (s *ConfigStore) updatePreferredModelFields(c *Config, modelType SelectedModelType, model SelectedModel) map[string]any {
	model = cloneSelectedModel(model)
	if c.Models == nil {
		c.Models = make(map[SelectedModelType]SelectedModel)
	}
	c.Models[modelType] = model
	c.markModelExplicit(modelType)

	fields := map[string]any{
		fmt.Sprintf("models.%s", modelType): model,
	}
	if updated, changed := nextRecentModels(c, modelType, model); changed {
		if c.RecentModels == nil {
			c.RecentModels = make(map[SelectedModelType][]SelectedModel)
		}
		c.RecentModels[modelType] = updated
		fields[fmt.Sprintf("recent_models.%s", modelType)] = updated
	}
	return fields
}

// SetCompactMode sets the compact mode setting and persists it.
func (s *ConfigStore) SetProviderDisabled(scope Scope, expected providerregistry.RegistrationOwner, disabled bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cfg := s.Config()
	if err := s.validateRegistrationOwnerLocked(cfg, expected); err != nil {
		return err
	}
	provider, configured := cfg.Providers.Get(expected.ProviderID)
	if !configured {
		var err error
		provider, err = s.providerConfigForCredentialLocked(cfg, expected.ProviderID)
		if err != nil {
			return err
		}
	}
	provider.Disable = disabled
	return s.updateLocked(scope, func(next *Config) map[string]any {
		next.Providers.Set(expected.ProviderID, provider)
		fields := map[string]any{
			fmt.Sprintf("providers.%s.disable", expected.ProviderID): disabled,
		}
		if provider.Owner != nil {
			fields[fmt.Sprintf("providers.%s.owner", expected.ProviderID)] = provider.Owner
		}
		if provider.Plugin != nil {
			fields[fmt.Sprintf("providers.%s.plugin", expected.ProviderID)] = provider.Plugin
		} else if provider.Preset != nil {
			fields[fmt.Sprintf("providers.%s.preset", expected.ProviderID)] = provider.Preset
		}
		return fields
	})
}

func (s *ConfigStore) SetCompactMode(scope Scope, enabled bool) error {
	return s.update(scope, func(c *Config) map[string]any {
		c.ensureTUI().CompactMode = enabled
		return map[string]any{"options.tui.compact_mode": enabled}
	})
}

// SetTransparentBackground sets the transparent background setting and persists it.
func (s *ConfigStore) SetTransparentBackground(scope Scope, enabled bool) error {
	return s.update(scope, func(c *Config) map[string]any {
		c.ensureTUI().Transparent = &enabled
		return map[string]any{"options.tui.transparent": enabled}
	})
}

// SetProviderAPIKey sets the API key for a provider and persists it.
func (s *ConfigStore) SetProviderAPIKey(scope Scope, providerID string, apiKey any) error {
	switch value := apiKey.(type) {
	case string:
		return fmt.Errorf("API key for provider %s is missing its initiating owner", providerID)
	case ProviderAPIKeyCredential:
		if value.Owner.ProviderID != providerID {
			return fmt.Errorf("API key owner provider %s does not match credential provider %s", value.Owner.ProviderID, providerID)
		}
		redact.Register(value.APIKey)
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		cfg := s.Config()
		current, ok := providerOwnerForConfig(cfg, s.providerRegistry, providerID)
		if !ok || current != value.Owner {
			return fmt.Errorf("registration owner for provider %s changed before the API key could be persisted", providerID)
		}
		providerConfig, err := s.providerConfigForCredentialLocked(cfg, providerID)
		if err != nil {
			return err
		}
		providerConfig.APIKey = value.APIKey
		return s.updateLocked(scope, func(cfg *Config) map[string]any {
			cfg.Providers.Set(providerID, providerConfig)
			fields := map[string]any{fmt.Sprintf("providers.%s.api_key", providerID): value.APIKey}
			if providerConfig.Owner != nil {
				fields[fmt.Sprintf("providers.%s.owner", providerID)] = providerConfig.Owner
			}
			if providerConfig.Plugin != nil {
				fields[fmt.Sprintf("providers.%s.plugin", providerID)] = providerConfig.Plugin
			} else if providerConfig.Preset != nil {
				fields[fmt.Sprintf("providers.%s.preset", providerID)] = providerConfig.Preset
			}
			return fields
		})
	case ProviderOAuthCredential:
		if value.Token == nil {
			return fmt.Errorf("OAuth token is nil")
		}
		if value.Owner.ProviderID != providerID {
			return fmt.Errorf("OAuth owner provider %s does not match credential provider %s", value.Owner.ProviderID, providerID)
		}
		return s.SetProviderOAuthToken(scope, value.Owner, value.Token)
	case *oauth.Token:
		return fmt.Errorf("OAuth token for provider %s is missing its initiating owner", providerID)
	default:
		return fmt.Errorf("unsupported API key type %T", apiKey)
	}
}

func (s *ConfigStore) providerConfigForCredentialLocked(cfg *Config, providerID string) (ProviderConfig, error) {
	providerConfig, configured := cfg.Providers.Get(providerID)
	if !configured {
		found := false
		for _, provider := range s.knownProviders {
			if string(provider.ID) != providerID {
				continue
			}
			providerConfig = ProviderConfig{
				ID:           providerID,
				Name:         provider.Name,
				BaseURL:      provider.APIEndpoint,
				Type:         provider.Type,
				ExtraHeaders: make(map[string]string),
				ExtraParams:  make(map[string]string),
				Models:       provider.Models,
			}
			found = true
			break
		}
		if !found {
			return ProviderConfig{}, fmt.Errorf("provider with ID %s not found in known providers", providerID)
		}
		if registration, registered := cfg.providerRegistration(s.providerRegistry, providerID); registered {
			providerConfig.Owner = providerOwnerReferenceForRegistration(registration)
			if registration.Manifest != nil {
				providerConfig.Plugin = &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}
			}
		} else if preset, active := cfg.providerPreset(s.providerRegistry, providerID); active {
			providerConfig.Owner = providerPresetOwnerReference()
			providerConfig.Preset = &preset
		}
	}
	prepared, err := prepareConfiguredProviderOwner(providerID, providerConfig)
	if err != nil {
		return ProviderConfig{}, err
	}
	prepared = cfg.completeProviderOwner(providerID, prepared)
	switch prepared.Owner.Type {
	case ProviderOwnerPlugin, ProviderOwnerCore:
		if _, active := providerRegistrationForProvider(s.providerRegistry, providerID, prepared); !active {
			return ProviderConfig{}, fmt.Errorf("provider owner for provider %s is not active", providerID)
		}
	case ProviderOwnerPreset:
		preset, active := activeProviderPresetForRegistry(cfg, s.providerRegistry, providerID)
		if !active || !providerPresetReferenceMatches(providerID, prepared.Preset, preset) {
			return ProviderConfig{}, fmt.Errorf("provider preset %s for provider %s is not active", prepared.Preset.ID, providerID)
		}
	case ProviderOwnerCustom:
	default:
		return ProviderConfig{}, fmt.Errorf("provider %s has invalid owner type %q", providerID, prepared.Owner.Type)
	}
	return prepared, nil
}

func (s *ConfigStore) SetProviderOAuthToken(scope Scope, expected providerregistry.RegistrationOwner, token *oauth.Token) error {
	if token == nil {
		return fmt.Errorf("OAuth token is nil")
	}
	redact.Register(token.AccessToken, token.RefreshToken)
	return s.withRefreshLock(expected.ProviderID, func() error {
		return s.setProviderOAuthTokenLocked(scope, expected.ProviderID, token, &expected)
	})
}

func (s *ConfigStore) setProviderOAuthTokenLocked(scope Scope, providerID string, token *oauth.Token, expected *providerregistry.RegistrationOwner) error {
	if token == nil {
		return fmt.Errorf("OAuth token is nil")
	}
	redact.Register(token.AccessToken, token.RefreshToken)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cfg := s.Config()
	registration, err := exactOAuthRegistrationFor(cfg, s.providerRegistry, providerID)
	if err != nil {
		return err
	}
	if expected != nil && !expected.Matches(registration) {
		return fmt.Errorf("OAuth owner for provider %s changed before the token could be persisted", providerID)
	}
	providerConfig, err := s.providerConfigForCredentialLocked(cfg, providerID)
	if err != nil {
		return err
	}
	applyOAuthTokenToProvider(&providerConfig, token, registration)
	return s.updateLocked(scope, func(next *Config) map[string]any {
		next.Providers.Set(providerID, providerConfig)
		fields := map[string]any{
			fmt.Sprintf("providers.%s.api_key", providerID): token.AccessToken,
			fmt.Sprintf("providers.%s.oauth", providerID):   token,
		}
		if providerConfig.Owner != nil {
			fields[fmt.Sprintf("providers.%s.owner", providerID)] = providerConfig.Owner
		}
		if providerConfig.Plugin != nil {
			fields[fmt.Sprintf("providers.%s.plugin", providerID)] = providerConfig.Plugin
		} else if providerConfig.Preset != nil {
			fields[fmt.Sprintf("providers.%s.preset", providerID)] = providerConfig.Preset
		}
		return fields
	})
}

func (s *ConfigStore) RemoveProviderCredentials(scope Scope, expected providerregistry.RegistrationOwner) error {
	if expected.ProviderID == "" {
		return fmt.Errorf("OAuth owner provider is empty")
	}
	return s.withRefreshLock(expected.ProviderID, func() error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()

		cfg := s.Config()
		registration, err := exactOAuthRegistrationFor(cfg, s.providerRegistry, expected.ProviderID)
		if err != nil {
			return err
		}
		if !expected.Matches(registration) {
			return fmt.Errorf("OAuth owner for provider %s changed before credentials could be removed", expected.ProviderID)
		}
		var hasForwardedAccount bool
		if expected.AccountNamespace != "" {
			s.configMu.RLock()
			forwarded, ok := s.ephemeralAccounts[expected.AccountNamespace]
			s.configMu.RUnlock()
			if ok && forwarded.Owner != expected {
				return fmt.Errorf("forwarded account owner for provider %s changed before credentials could be removed", expected.ProviderID)
			}
			hasForwardedAccount = ok
		}
		providerConfig, ok := cfg.Providers.Get(expected.ProviderID)
		if !ok {
			return fmt.Errorf("provider %s not found", expected.ProviderID)
		}
		providerConfig.APIKey = ""
		providerConfig.APIKeyTemplate = ""
		providerConfig.OAuthToken = nil
		next := cfg.cloneForWrite()
		next.Providers.Set(expected.ProviderID, providerConfig)

		if s.isEphemeralProvider(expected.ProviderID) {
			registerConfigSecrets(next)
			s.configMu.Lock()
			s.config = next
			if s.ephemeralProviderConfigs == nil {
				s.ephemeralProviderConfigs = make(map[string]ProviderConfig)
			}
			s.ephemeralProviderConfigs[expected.ProviderID] = providerConfig
			if hasForwardedAccount {
				delete(s.ephemeralAccounts, expected.AccountNamespace)
			}
			s.configMu.Unlock()
			return nil
		}

		keys := []string{
			fmt.Sprintf("providers.%s.api_key", expected.ProviderID),
			fmt.Sprintf("providers.%s.oauth", expected.ProviderID),
		}
		if err := s.atomicWrite(scope, func(data []byte) ([]byte, error) {
			value := string(data)
			for _, key := range keys {
				value, err = sjson.Delete(value, key)
				if err != nil {
					return nil, fmt.Errorf("failed to delete config field %s: %w", key, err)
				}
			}
			return []byte(value), nil
		}); err != nil {
			return err
		}
		s.setConfig(next)
		if path, err := s.configPath(scope); err == nil {
			s.captureStalenessSnapshot(append(slices.Clone(s.loadedPaths), path))
		}
		return nil
	})
}

// RefreshOAuthTokenForOwner refreshes the OAuth token for the given provider owner.
//
// Providers may rotate refresh tokens: each exchange consumes the
// caller's refresh token, issues a new pair, and revokes the old one. If
// two crux instances (or two goroutines) refresh concurrently with the
// same stored refresh token, the second exchange reuses an already-revoked
// token, trips the provider's reuse detection, and revokes the entire
// token family — leaving both with dead tokens even though each refresh
// "succeeded".
//
// To prevent that, refreshes are single-flighted at two levels:
//
//   - In-process: refreshSF collapses concurrent goroutines for the same
//     provider into one attempt.
//   - Cross-process: a per-provider advisory lock is held across the whole
//     read-decide-exchange-write cycle, so only one process exchanges at a
//     time. A process that acquires the lock after a peer rotated finds the
//     peer's fresh token on disk and adopts it instead of exchanging.
func (s *ConfigStore) RefreshOAuthTokenForOwner(ctx context.Context, scope Scope, expected providerregistry.RegistrationOwner) (*oauth.Token, error) {
	if expected.ProviderID == "" {
		return nil, fmt.Errorf("OAuth owner provider is empty")
	}
	key := fmt.Sprintf("%d\x00%#v", scope, expected)
	value, err, _ := s.refreshSF.Do(key, func() (any, error) {
		return s.refreshOAuthTokenLocked(ctx, scope, expected)
	})
	if err != nil {
		return nil, err
	}
	token, ok := value.(*oauth.Token)
	if !ok || token == nil {
		return nil, fmt.Errorf("OAuth refresh for provider %s returned no token", expected.ProviderID)
	}
	return token, nil
}

// refreshOAuthTokenLocked performs the cross-process single-flighted
// refresh. It is invoked through refreshSF, so at most one goroutine per
// provider runs it at a time within this process.
func (s *ConfigStore) refreshOAuthTokenLocked(ctx context.Context, scope Scope, expected providerregistry.RegistrationOwner) (*oauth.Token, error) {
	providerID := expected.ProviderID
	if err := s.ValidateRegistrationOwner(expected); err != nil {
		return nil, err
	}
	cfg := s.Config()
	providerConfig, exists := cfg.Providers.Get(providerID)
	if !exists {
		return nil, fmt.Errorf("provider %s not found", providerID)
	}
	if providerConfig.OAuthToken == nil {
		return nil, fmt.Errorf("provider %s does not have an OAuth token", providerID)
	}
	registration, err := s.exactOAuthRegistration(cfg, providerID)
	if err != nil {
		return nil, err
	}
	if !expected.Matches(registration) {
		return nil, fmt.Errorf("OAuth owner for provider %s changed before token refresh", providerID)
	}
	if registration.OAuth.Refresh == nil {
		return nil, fmt.Errorf("OAuth refresh not supported for provider %s", providerID)
	}
	entryToken := providerConfig.OAuthToken
	if s.isEphemeralProvider(providerID) {
		refreshedToken, err := s.exchange(ctx, providerID, entryToken.RefreshToken, expected)
		if err != nil {
			return nil, fmt.Errorf("failed to refresh ephemeral OAuth token for provider %s: %w", providerID, err)
		}
		if err := s.applyEphemeralToken(refreshedToken, expected); err != nil {
			return nil, err
		}
		return refreshedToken, nil
	}

	lockCtx, cancel := context.WithTimeout(ctx, refreshLockDeadline)
	defer cancel()
	release, lockErr := lock.File(lockCtx, s.refreshLockPath(providerID))
	if lockErr != nil {
		if diskToken := s.usableDiskToken(scope, providerID, entryToken); diskToken != nil {
			slog.Warn("Refresh lock unavailable; adopting token from disk", "provider", providerID, "error", lockErr)
			if err := s.applyToken(diskToken, providerID, expected); err != nil {
				return nil, err
			}
			return diskToken, nil
		}
		return nil, fmt.Errorf("acquire refresh lock for provider %s: %w", providerID, lockErr)
	}
	defer release()

	if diskToken := s.newerDiskToken(scope, providerID, entryToken); diskToken != nil {
		if !diskToken.IsExpired() {
			slog.Info("Adopting token refreshed by another session", "provider", providerID)
			if err := s.applyToken(diskToken, providerID, expected); err != nil {
				return nil, err
			}
			return diskToken, nil
		}
		slog.Info("Exchanging with refresh token rotated by another session", "provider", providerID)
		entryToken = diskToken
	}

	refreshedToken, refreshErr := s.exchange(ctx, providerID, entryToken.RefreshToken, expected)
	if refreshErr != nil {
		if diskToken := s.newerDiskToken(scope, providerID, entryToken); diskToken != nil {
			if !diskToken.IsExpired() {
				slog.Info("Adopting token refreshed by another session after exchange failure", "provider", providerID)
				if err := s.applyToken(diskToken, providerID, expected); err != nil {
					return nil, err
				}
				return diskToken, nil
			}
			slog.Info("Retrying exchange with refresh token rotated by another session", "provider", providerID)
			refreshedToken, refreshErr = s.exchange(ctx, providerID, diskToken.RefreshToken, expected)
		}
	}
	if refreshErr != nil {
		return nil, fmt.Errorf("failed to refresh OAuth token for provider %s: %w", providerID, refreshErr)
	}

	slog.Info("Successfully refreshed OAuth token", "provider", providerID)
	if err := s.setProviderOAuthTokenLocked(scope, providerID, refreshedToken, &expected); err != nil {
		return nil, fmt.Errorf("failed to persist refreshed token: %w", err)
	}
	return refreshedToken, nil
}

// WaitForTokenChange blocks until SignalAuthComplete is called for the
// given provider or the context is cancelled. It is used by OnAuthRefresh
// callbacks to wait for interactive re-authentication to complete before
// retrying a failed request. The channel is created atomically with the
// wait registration so a concurrent SignalAuthComplete cannot miss it.
func (s *ConfigStore) WaitForTokenChange(ctx context.Context, owner providerregistry.RegistrationOwner) error {
	s.authSignalMu.Lock()
	ch, ok := s.authSignals[owner]
	if !ok {
		ch = make(chan struct{})
		if s.authSignals == nil {
			s.authSignals = make(map[providerregistry.RegistrationOwner]chan struct{})
		}
		s.authSignals[owner] = ch
	}
	s.authSignalMu.Unlock()

	select {
	case <-ch:
		// Remove the consumed signal so a subsequent
		// SignalAuthComplete does not close an already-closed
		// channel.
		s.authSignalMu.Lock()
		if s.authSignals[owner] == ch {
			delete(s.authSignals, owner)
		}
		s.authSignalMu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SignalAuthComplete unblocks any goroutine waiting in WaitForTokenChange
// for the given provider. If no waiter exists yet, it pre-creates and
// immediately closes the channel so a subsequent WaitForTokenChange
// returns without blocking. This eliminates the race where the signal
// fires before the waiter registers.
func (s *ConfigStore) SignalAuthComplete(owner providerregistry.RegistrationOwner) {
	s.authSignalMu.Lock()
	defer s.authSignalMu.Unlock()
	if ch, ok := s.authSignals[owner]; ok {
		delete(s.authSignals, owner)
		select {
		case <-ch:
			// Already closed by a previous signal; nothing to do.
		default:
			close(ch)
		}
	} else {
		// No waiter yet. Pre-create a closed channel so the next
		// WaitForTokenChange returns immediately.
		if s.authSignals == nil {
			s.authSignals = make(map[providerregistry.RegistrationOwner]chan struct{})
		}
		ch := make(chan struct{})
		close(ch)
		s.authSignals[owner] = ch
	}
}

// newerDiskToken returns the on-disk token for the provider when it is
// newer than entryToken — i.e. another session (possibly in another
// process) has already rotated the credential. It returns nil when disk
// holds nothing newer than what we started with.
//
// Newness is judged by expiry as well as identity, so a config file that
// somehow holds an older token cannot drag us backwards. The result may
// itself be expired: providers that rotate refresh tokens invalidate ours
// the moment a peer refreshes, so the peer's refresh token is the only one
// the provider will still accept even after its access token ages out.
// Callers decide whether to adopt the token wholesale or merely borrow its
// refresh token.
func (s *ConfigStore) newerDiskToken(scope Scope, providerID string, entryToken *oauth.Token) *oauth.Token {
	diskToken, err := s.loadTokenFromDisk(scope, providerID)
	if err != nil {
		slog.Warn("Failed to read token from config file", "provider", providerID, "error", err)
		return nil
	}
	if diskToken == nil {
		return nil
	}
	if diskToken.AccessToken == entryToken.AccessToken {
		// Same token we started with; nobody rotated since.
		return nil
	}
	if diskToken.RefreshToken == "" && entryToken.RefreshToken != "" {
		// Adopting would strand us with no way to refresh later, and
		// there is nothing to borrow for an exchange.
		return nil
	}
	if diskToken.ExpiresAt < entryToken.ExpiresAt {
		// Older than ours; nothing to gain from adopting it.
		return nil
	}
	return diskToken
}

// usableDiskToken returns the on-disk token only when it is both newer
// than entryToken and still valid, meaning it can be adopted as-is with
// no exchange at all.
func (s *ConfigStore) usableDiskToken(scope Scope, providerID string, entryToken *oauth.Token) *oauth.Token {
	diskToken := s.newerDiskToken(scope, providerID, entryToken)
	if diskToken == nil || diskToken.IsExpired() {
		return nil
	}
	return diskToken
}

// exchange performs the provider-specific OAuth token exchange. Tests may
// override it via the exchangeToken field; production uses the real
// provider clients.
func (s *ConfigStore) exchange(ctx context.Context, providerID, refreshToken string, expected providerregistry.RegistrationOwner) (*oauth.Token, error) {
	registration, err := s.exactOAuthRegistration(s.Config(), providerID)
	if err != nil {
		return nil, err
	}
	if !expected.Matches(registration) {
		return nil, fmt.Errorf("OAuth owner for provider %s changed before token refresh", providerID)
	}
	if registration.OAuth.Refresh == nil {
		return nil, fmt.Errorf("OAuth refresh not supported for provider %s", providerID)
	}
	ctx = providertransport.ContextWithOwnerValidator(ctx, func() error {
		return s.ValidateRegistrationOwner(expected)
	})
	var token *oauth.Token
	if s.exchangeToken != nil {
		token, err = s.exchangeToken(ctx, providerID, refreshToken)
	} else {
		token, err = registration.OAuth.Refresh(ctx, refreshToken)
	}
	if ownerErr := s.ValidateRegistrationOwner(expected); ownerErr != nil {
		return nil, fmt.Errorf("OAuth owner for provider %s is not active after token refresh: %w", providerID, ownerErr)
	}
	return token, err
}

// withRefreshLock runs fn while holding the per-provider cross-process
// refresh lock, so a credential write cannot interleave with a peer's
// token exchange. Acquisition is best effort: when the lock cannot be
// taken in time, fn runs anyway rather than blocking a write the user is
// waiting on.
func (s *ConfigStore) withRefreshLock(providerID string, fn func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), credentialWriteLockDeadline)
	defer cancel()
	release, err := lock.File(ctx, s.refreshLockPath(providerID))
	if err != nil {
		slog.Warn("Writing credentials without the refresh lock", "provider", providerID, "error", err)
		return fn()
	}
	defer release()
	return fn()
}

// refreshLockPath returns the path to the per-provider cross-process refresh
// lock file. Lock files live under a dedicated locks/ subdirectory of the
// data dir so they do not clutter the config directory. The file is created
// on demand by lock.File and is never removed (flock keys on inode, not
// path).
func (s *ConfigStore) refreshLockPath(providerID string) string {
	dir := filepath.Join(filepath.Dir(s.globalDataPath), "locks")
	_ = os.MkdirAll(dir, 0o755)
	digest := sha256.Sum256([]byte(providerID))
	return filepath.Join(dir, fmt.Sprintf("%x.refresh.lock", digest))
}

// applyToken updates the in-memory provider config with the given token.
func (s *ConfigStore) applyToken(token *oauth.Token, providerID string, expected providerregistry.RegistrationOwner) error {
	if token == nil {
		return fmt.Errorf("OAuth token is nil")
	}
	redact.Register(token.AccessToken, token.RefreshToken)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cfg := s.Config()
	registration, err := exactOAuthRegistrationFor(cfg, s.providerRegistry, providerID)
	if err != nil {
		return err
	}
	if !expected.Matches(registration) {
		return fmt.Errorf("OAuth owner for provider %s changed before the token could be applied", providerID)
	}
	providerConfig, ok := cfg.Providers.Get(providerID)
	if !ok {
		return fmt.Errorf("provider %s not found", providerID)
	}
	applyOAuthTokenToProvider(&providerConfig, token, registration)
	next := cfg.cloneForWrite()
	next.Providers.Set(providerID, providerConfig)
	s.setConfig(next)
	return nil
}

// loadTokenFromDisk reads the OAuth token for the given provider from the
// config file on disk. Returns nil if the token is not found or matches the
// current in-memory token.
func (s *ConfigStore) loadTokenFromDisk(scope Scope, providerID string) (*oauth.Token, error) {
	path, err := s.configPath(scope)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	oauthKey := fmt.Sprintf("providers.%s.oauth", providerID)
	oauthResult := gjson.Get(string(data), oauthKey)
	if !oauthResult.Exists() {
		return nil, nil
	}

	var token oauth.Token
	if err := json.Unmarshal([]byte(oauthResult.Raw), &token); err != nil {
		return nil, err
	}

	if token.AccessToken == "" {
		return nil, nil
	}

	return &token, nil
}

// nextRecentModels computes the recent-models list for the given type
// after recording the supplied model at the front, operating on the
// provided config without persisting anything. It returns the new slice
// and whether it differs from cfg's current list. Callers fold the result
// into a clone they are about to publish.
func nextRecentModels(cfg *Config, modelType SelectedModelType, model SelectedModel) ([]SelectedModel, bool) {
	if model.Provider == "" || model.Model == "" {
		return nil, false
	}

	eq := func(a, b SelectedModel) bool {
		return a.Provider == b.Provider && a.Model == b.Model
	}

	entry := SelectedModel{
		Provider: model.Provider,
		Model:    model.Model,
	}

	current := cfg.RecentModels[modelType]
	withoutCurrent := slices.DeleteFunc(slices.Clone(current), func(existing SelectedModel) bool {
		return eq(existing, entry)
	})

	updated := append([]SelectedModel{entry}, withoutCurrent...)
	if len(updated) > maxRecentModelsPerType {
		updated = updated[:maxRecentModelsPerType]
	}

	if slices.EqualFunc(current, updated, eq) {
		return current, false
	}

	return updated, true
}

// NewTestStore creates a ConfigStore for testing purposes.
func NewTestStore(cfg *Config, loadedPaths ...string) *ConfigStore {
	registry, _ := providerregistry.New(providerregistry.Integrated()...)
	if cfg != nil {
		if cfg.providerScan != nil {
			registry = cfg.providerScan.Registry.Clone()
		} else {
			cfg.bindProviderScan(ProviderScan{Registry: registry})
		}
	}
	return newTestStoreWithRegistry(cfg, registry, loadedPaths...)
}

func NewTestStoreWithRegistrations(cfg *Config, registrations ...providerregistry.Registration) *ConfigStore {
	return NewTestStoreWithProviderGeneration(cfg, nil, registrations...)
}

func NewTestStoreWithProviderGeneration(cfg *Config, presets map[string]ProviderPresetReference, registrations ...providerregistry.Registration) *ConfigStore {
	registry, _ := providerregistry.New(registrations...)
	if cfg != nil {
		cfg.bindProviderScan(ProviderScan{Registry: registry, presetReferences: maps.Clone(presets)})
	}
	return newTestStoreWithRegistry(cfg, registry)
}

func newTestStoreWithRegistry(cfg *Config, registry *providerregistry.Registry, loadedPaths ...string) *ConfigStore {
	baseEnvironment := snapshotEnvironment()
	return &ConfigStore{
		config:               cfg,
		loadedPaths:          loadedPaths,
		resolver:             NewShellVariableResolver(env.New()),
		baseEnvironment:      baseEnvironment,
		effectiveEnvironment: cloneEnvironment(baseEnvironment),
		providerRegistry:     registry,
	}
}

// ImportCopilot attempts to import a GitHub Copilot token from disk.
func (s *ConfigStore) ImportCopilot() (*oauth.Token, bool) {
	providerID := string(catalog.ProviderCopilot)
	registration, err := s.exactOAuthRegistration(s.Config(), providerID)
	if err != nil || registration.Construction != providerregistry.ConstructionCopilot || registration.OAuth.Import == nil {
		return nil, false
	}
	owner := registration.Owner()
	if s.HasConfigField(ScopeGlobal, "providers.copilot.api_key") || s.HasConfigField(ScopeGlobal, "providers.copilot.oauth") {
		return nil, false
	}

	ctx := providertransport.ContextWithOwnerValidator(context.TODO(), func() error {
		return s.ValidateRegistrationOwner(owner)
	})
	token, found, err := registration.OAuth.Import(ctx)
	if ownerErr := s.ValidateRegistrationOwner(owner); ownerErr != nil {
		slog.Error("Unable to import GitHub Copilot token", "error", ownerErr)
		return nil, false
	}
	if err != nil {
		slog.Error("Unable to import GitHub Copilot token", "error", err)
		return nil, false
	}
	if !found || token == nil {
		return nil, false
	}

	slog.Info("Found existing GitHub Copilot token on disk. Authenticating...")
	if err := s.SetProviderOAuthToken(ScopeGlobal, owner, token); err != nil {
		return nil, false
	}

	slog.Info("GitHub Copilot successfully imported")
	return token, true
}

// StalenessResult contains the result of a staleness check.
type StalenessResult struct {
	Dirty   bool
	Changed []string
	Missing []string
	Errors  map[string]error // stat errors by path
}

// ConfigStaleness checks whether any tracked config files have changed on disk
// since the last snapshot. Returns dirty=true if any files changed or went
// missing, along with sorted lists of affected paths. Stat errors are
// captured in Errors map but still treated as non-existence for dirty detection.
func (s *ConfigStore) ConfigStaleness() StalenessResult {
	var result StalenessResult
	result.Errors = make(map[string]error)

	for _, path := range s.trackedConfigPaths {
		snapshot, hadSnapshot := s.snapshots[path]

		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()

		if err != nil && !os.IsNotExist(err) {
			// Capture permission/IO errors separately from non-existence
			result.Errors[path] = err
			result.Dirty = true
		}

		if !exists {
			if hadSnapshot && snapshot.Exists {
				// File existed before but now missing
				result.Missing = append(result.Missing, path)
				result.Dirty = true
			}
			continue
		}

		// File exists now
		if !hadSnapshot || !snapshot.Exists {
			// File didn't exist before but does now
			result.Changed = append(result.Changed, path)
			result.Dirty = true
			continue
		}

		// Check for content or metadata changes
		if snapshot.Size != info.Size() || snapshot.ModTime != info.ModTime().UnixNano() {
			result.Changed = append(result.Changed, path)
			result.Dirty = true
		}
	}

	// Sort for deterministic output
	slices.Sort(result.Changed)
	slices.Sort(result.Missing)

	return result
}

// RefreshStalenessSnapshot captures fresh snapshots of all tracked config files.
// Call this after reloading config to clear dirty state.
func (s *ConfigStore) RefreshStalenessSnapshot() error {
	if s.snapshots == nil {
		s.snapshots = make(map[string]fileSnapshot)
	}

	for _, path := range s.trackedConfigPaths {
		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()

		snapshot := fileSnapshot{
			Path:   path,
			Exists: exists,
		}

		if exists {
			snapshot.Size = info.Size()
			snapshot.ModTime = info.ModTime().UnixNano()
		}

		s.snapshots[path] = snapshot
	}

	return nil
}

// CaptureStalenessSnapshot captures snapshots for the given paths, building the
// tracked config paths list. Paths are deduplicated and normalized.
func (s *ConfigStore) CaptureStalenessSnapshot(paths []string) {
	// Build unique set of normalized paths
	seen := make(map[string]struct{})
	for _, p := range paths {
		if p == "" {
			continue
		}
		// Normalize path
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		seen[abs] = struct{}{}
	}

	// Also track workspace and global config paths if set
	if s.workspacePath != "" {
		abs, err := filepath.Abs(s.workspacePath)
		if err == nil {
			seen[abs] = struct{}{}
		}
	}
	if s.globalDataPath != "" {
		abs, err := filepath.Abs(s.globalDataPath)
		if err == nil {
			seen[abs] = struct{}{}
		}
	}

	// Build sorted list for deterministic ordering
	s.trackedConfigPaths = make([]string, 0, len(seen))
	for p := range seen {
		s.trackedConfigPaths = append(s.trackedConfigPaths, p)
	}
	slices.Sort(s.trackedConfigPaths)

	// Capture initial snapshots
	s.RefreshStalenessSnapshot()
}

// captureStalenessSnapshot is an alias for CaptureStalenessSnapshot for internal use.
func (s *ConfigStore) captureStalenessSnapshot(paths []string) {
	s.CaptureStalenessSnapshot(paths)
}

// ReloadFromDisk re-runs the config load/merge flow and updates the in-memory
// config atomically. It rebuilds the staleness snapshot after successful reload.
// On failure, the store state is rolled back to its previous state.
// Concurrent calls are serialised via writeMu.
func (s *ConfigStore) ReloadFromDisk(ctx context.Context) error {
	if s.workingDir == "" {
		return fmt.Errorf("cannot reload: working directory not set")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.reloadFromDiskLocked(ctx)
}

type reloadSelectedValues struct {
	models   map[SelectedModelType]SelectedModel
	explicit map[SelectedModelType]bool
}

type reloadProviderOwners struct {
	owners  map[string]ProviderOwnerReference
	plugins map[string]ProviderPluginReference
	presets map[string]ProviderPresetReference
}

func captureReloadSelectedValues(cfg *Config) reloadSelectedValues {
	result := reloadSelectedValues{
		models:   make(map[SelectedModelType]SelectedModel, len(cfg.Models)),
		explicit: maps.Clone(cfg.explicitModels),
	}
	for modelType, model := range cfg.Models {
		result.models[modelType] = cloneSelectedModel(model)
	}
	return result
}

func captureReloadProviderOwners(cfg *Config) reloadProviderOwners {
	result := reloadProviderOwners{
		owners:  make(map[string]ProviderOwnerReference),
		plugins: make(map[string]ProviderPluginReference),
		presets: make(map[string]ProviderPresetReference),
	}
	if cfg == nil || cfg.Providers == nil {
		return result
	}
	for providerID, provider := range cfg.Providers.Seq2() {
		provider = cfg.completeProviderOwner(providerID, provider)
		if provider.Owner != nil {
			result.owners[providerID] = *provider.Owner
		}
		if provider.Plugin != nil {
			result.plugins[providerID] = *provider.Plugin
		}
		if provider.Preset != nil {
			result.presets[providerID] = *provider.Preset
		}
	}
	return result
}

func providerScanRegistrationOwners(scan ProviderScan) []providerregistry.RegistrationOwner {
	if scan.Registry == nil {
		return nil
	}
	registrations := scan.Registry.Registrations()
	owners := make([]providerregistry.RegistrationOwner, len(registrations))
	for index, registration := range registrations {
		owners[index] = registration.Owner()
	}
	return owners
}

func reloadProviderScansMatch(expected, current ProviderScan) bool {
	if expected.Registry == nil != (current.Registry == nil) {
		return false
	}
	return reflect.DeepEqual(expected.Providers, current.Providers) &&
		reflect.DeepEqual(expected.pluginStatuses, current.pluginStatuses) &&
		reflect.DeepEqual(expected.presetReferences, current.presetReferences) &&
		reflect.DeepEqual(expected.ownerModes, current.ownerModes) &&
		reflect.DeepEqual(providerScanRegistrationOwners(expected), providerScanRegistrationOwners(current))
}

func (s *ConfigStore) loadReloadConfigInputs(
	ctx context.Context,
	configPaths []string,
	fileOverrides map[string][]byte,
	baseEnvironment env.Env,
	dataDir string,
	ephemeralProviders map[string]ProviderConfig,
	overrides RuntimeOverrides,
	validateEphemeral bool,
) (*Config, []string, string, error) {
	cfg, loadedPaths, err := loadFromConfigPathsWithOverrides(ctx, configPaths, fileOverrides, baseEnvironment)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to reload config: %w", err)
	}
	if err := cfg.setDefaultsFromEnvironment(s.workingDir, dataDir, baseEnvironment); err != nil {
		return nil, nil, "", fmt.Errorf("apply configuration defaults during reload: %w", err)
	}

	workspacePath := filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName))
	if workspaceData, err := os.ReadFile(workspacePath); err == nil && len(workspaceData) > 0 {
		if !json.Valid(workspaceData) {
			return nil, nil, "", fmt.Errorf("invalid JSON in config file %s", workspacePath)
		}
		merged, mergeErr := loadFromBytes(append([][]byte{mustMarshalConfig(cfg)}, workspaceData))
		if mergeErr == nil {
			configuredDataDir := cfg.Options.DataDirectory
			*cfg = *merged
			if err := cfg.setDefaultsFromEnvironment(s.workingDir, configuredDataDir, baseEnvironment); err != nil {
				return nil, nil, "", fmt.Errorf("apply workspace configuration defaults during reload: %w", err)
			}
			loadedPaths = append(loadedPaths, workspacePath)
		}
	}

	if len(ephemeralProviders) > 0 {
		if validateEphemeral {
			if err := validateForwardedProviderState(cfg, ephemeralProviders); err != nil {
				return nil, nil, "", fmt.Errorf("validate ephemeral provider state during reload: %w", err)
			}
		}
		overlay, err := json.Marshal(struct {
			Providers map[string]ProviderConfig `json:"providers"`
		}{Providers: ephemeralProviders})
		if err != nil {
			return nil, nil, "", fmt.Errorf("encode ephemeral provider state during reload: %w", err)
		}
		merged, err := loadFromBytes([][]byte{mustMarshalConfig(cfg), overlay})
		if err != nil {
			return nil, nil, "", fmt.Errorf("apply ephemeral provider state during reload: %w", err)
		}
		configuredDataDir := cfg.Options.DataDirectory
		cfg = merged
		if err := cfg.setDefaultsFromEnvironment(s.workingDir, configuredDataDir, baseEnvironment); err != nil {
			return nil, nil, "", fmt.Errorf("apply ephemeral provider defaults during reload: %w", err)
		}
	}
	cfg.captureExplicitModels()

	if err := cfg.ValidateHooks(); err != nil {
		return nil, nil, "", fmt.Errorf("invalid hook configuration on reload: %w", err)
	}
	if err := cfg.Options.validatePromptOptions(); err != nil {
		return nil, nil, "", fmt.Errorf("invalid prompt options on reload: %w", err)
	}

	for modelType, model := range overrides.Models {
		cfg.Models[modelType] = cloneSelectedModel(model)
		cfg.markModelExplicit(modelType)
	}
	return cfg, loadedPaths, workspacePath, nil
}

var (
	captureReloadConfigPostimages = captureConfigPostimages
	publishReloadProviderScan     = publishConfiguredProviderScan
)

func (s *ConfigStore) revalidateReloadGeneration(
	ctx context.Context,
	configPaths []string,
	baseEnvironment env.Env,
	dataDir string,
	ephemeralProviders map[string]ProviderConfig,
	overrides RuntimeOverrides,
	expectedSelected reloadSelectedValues,
	expectedOwners reloadProviderOwners,
	expectedScan ProviderScan,
) error {
	currentPaths := lookupConfigsFromEnvironment(s.workingDir, baseEnvironment)
	if !slices.Equal(configPaths, currentPaths) {
		return errors.New("configuration sources changed before publication")
	}
	cfg, _, _, err := s.loadReloadConfigInputs(ctx, currentPaths, nil, baseEnvironment, dataDir, ephemeralProviders, overrides, false)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expectedSelected, captureReloadSelectedValues(cfg)) {
		return errors.New("selected model values changed before publication")
	}
	currentScan, err := freshProviderScan(ctx, cfg, baseEnvironment)
	if err != nil {
		return fmt.Errorf("rescan provider trust and compatibility: %w", err)
	}
	cfg.bindProviderScan(currentScan)
	if !reflect.DeepEqual(expectedOwners, captureReloadProviderOwners(cfg)) {
		return errors.New("persisted provider owner changed before publication")
	}
	if !reloadProviderScansMatch(expectedScan, currentScan) {
		return errors.New("provider trust, compatibility, or exact owner generation changed before publication")
	}
	return nil
}

// reloadFromDiskLocked performs the actual reload. Caller must hold writeMu.
// Reload has the same provider-plugin retention contract as startup: an absent
// registration must not replace an explicit selected provider, model, or any
// provider-specific model values. Keep resolution before SetupAgents and roll
// back on real setup errors without normalizing unavailable selections.
func (s *ConfigStore) reloadFromDiskLocked(ctx context.Context) error {
	baseEnvironment := s.baseEnvironment
	if baseEnvironment == nil {
		baseEnvironment = snapshotEnvironment()
	}
	globalConfigPath := globalConfigFromEnvironment(baseEnvironment)
	globalDataPath := globalConfigDataFromEnvironment(appName, baseEnvironment)
	notificationMigration := prepareDisableNotificationsMigration(globalConfigPath, globalDataPath)
	preimages, err := captureConfigPreimages(globalConfigPath, globalDataPath)
	if err != nil {
		return fmt.Errorf("capture reload config state: %w", err)
	}
	configPaths := lookupConfigsFromEnvironment(s.workingDir, baseEnvironment)
	var dataDir string
	if current := s.Config(); current != nil && current.Options != nil {
		dataDir = current.Options.DataDirectory
	}
	ephemeralProviders := s.ephemeralProviderSnapshot()
	overrides := cloneRuntimeOverrides(s.overrides)
	cfg, loadedPaths, workspacePath, err := s.loadReloadConfigInputs(
		ctx,
		configPaths,
		notificationMigration.overrides,
		baseEnvironment,
		dataDir,
		ephemeralProviders,
		overrides,
		true,
	)
	if err != nil {
		return err
	}
	expectedSelected := captureReloadSelectedValues(cfg)

	candidateEnv, resolver, resolvedEnv, environmentErr := cfg.buildEnvironmentFrom(baseEnvironment)
	if environmentErr != nil {
		return fmt.Errorf("build candidate environment during reload: %w", environmentErr)
	}

	scan, err := freshProviderScan(ctx, cfg, baseEnvironment)
	if err != nil {
		return fmt.Errorf("failed to scan providers during reload: %w", err)
	}
	cfg.bindProviderScan(scan)
	expectedOwners := captureReloadProviderOwners(cfg)
	providers := cloneProviderCatalog(scan.Providers)

	var pendingOwners map[string]ProviderOwnerReference
	var pendingPlugins map[string]ProviderPluginReference
	var pendingPresets map[string]ProviderPresetReference
	collectMigration := func(owners map[string]ProviderOwnerReference, plugins map[string]ProviderPluginReference, presets map[string]ProviderPresetReference) error {
		pendingOwners = maps.Clone(owners)
		pendingPlugins = maps.Clone(plugins)
		pendingPresets = maps.Clone(presets)
		return nil
	}
	if err := cfg.configureProvidersWithMigration(ctx, s, candidateEnv, resolver, providers, collectMigration); err != nil {
		return fmt.Errorf("failed to configure providers during reload: %w", err)
	}

	if !cfg.IsConfigured() {
		slog.Warn("No providers configured after reload")
	} else {
		resolved, resolveErr := resolveSelectedModels(cfg, providers)
		if resolveErr != nil {
			return fmt.Errorf("failed to configure selected models during reload: %w", resolveErr)
		}
		cfg.Models[SelectedModelTypeLarge] = resolved.Large
		cfg.Models[SelectedModelTypeSmall] = resolved.Small
	}
	cfg.SetupAgents()

	runtimeCandidate, err := s.prepareRuntimeGeneration(ctx, s.runtimeSnapshotLocked(cfg, resolver, scan.Registry, candidateEnv))
	if err != nil {
		return fmt.Errorf("prepare reloaded runtime generation: %w", err)
	}
	if runtimeCandidate.Abort != nil {
		defer runtimeCandidate.Abort()
	}

	if err := commitStartupCorrections(s, notificationMigration, nil, preimages); err != nil {
		rollbackErr := restoreConfigPreimages(preimages)
		return errors.Join(fmt.Errorf("commit reload config corrections: %w", err), rollbackErr)
	}
	if err := captureReloadConfigPostimages(preimages); err != nil {
		rollbackErr := restoreConfigPreimages(preimages)
		return errors.Join(fmt.Errorf("capture reload config correction postimages: %w", err), rollbackErr)
	}
	migrationExpectedData, migrationExpectedExists, err := configPostimage(preimages, globalDataPath)
	if err != nil {
		rollbackErr := restoreConfigPreimages(preimages)
		return errors.Join(fmt.Errorf("resolve reload provider ownership migration postimage: %w", err), rollbackErr)
	}
	_, migrationBeforeHash, migrationBeforeExists, err := fileBytesAndHash(globalDataPath)
	if err != nil {
		rollbackErr := restoreConfigPreimages(preimages)
		return errors.Join(fmt.Errorf("capture reload provider ownership migration preimage: %w", err), rollbackErr)
	}
	if err := s.migrateProviderReferencesIfCurrent(pendingOwners, pendingPlugins, pendingPresets, migrationExpectedData, migrationExpectedExists); err != nil {
		rollbackErr := restoreConfigPreimages(preimages)
		return errors.Join(fmt.Errorf("migrate provider ownership during reload: %w", err), rollbackErr)
	}
	_, migrationAfterHash, migrationAfterExists, migrationInspectErr := fileBytesAndHash(globalDataPath)
	ownershipChanged := migrationBeforeExists != migrationAfterExists || migrationBeforeHash != migrationAfterHash
	if migrationInspectErr != nil {
		var migrationRollbackErr error
		if ownershipChanged {
			migrationRollbackErr = s.rollbackProviderMigration()
		}
		rollbackErr := restoreConfigPreimages(preimages)
		return errors.Join(fmt.Errorf("inspect reload provider ownership migration: %w", migrationInspectErr), migrationRollbackErr, rollbackErr)
	}
	publish := func(published ProviderScan) error {
		if err := s.revalidateReloadGeneration(
			ctx,
			configPaths,
			baseEnvironment,
			dataDir,
			ephemeralProviders,
			overrides,
			expectedSelected,
			expectedOwners,
			scan,
		); err != nil {
			return fmt.Errorf("revalidate reloaded configuration generation: %w", err)
		}
		if s.publishProcessState {
			if err := s.applyProcessEnvironment(baseEnvironment, s.appliedEnvironment, resolvedEnv); err != nil {
				return err
			}
			s.appliedEnvironment = maps.Clone(resolvedEnv)
		}
		s.baseEnvironment = baseEnvironment
		s.effectiveEnvironment = cloneEnvironment(candidateEnv)
		s.globalDataPath = globalDataPath
		registerConfigSecrets(cfg)
		s.loadedPaths = loadedPaths
		s.resolver = resolver
		s.knownProviders = cloneProviderCatalog(published.Providers)
		if published.Registry == nil {
			s.providerRegistry = nil
		} else {
			s.providerRegistry = published.Registry.Clone()
		}
		s.overrides = overrides
		s.workspacePath = workspacePath
		s.configMu.Lock()
		s.config = cfg
		s.configMu.Unlock()
		if runtimeCandidate.Commit != nil {
			runtimeCandidate.Commit()
		}
		return nil
	}
	var publishErr error
	if s.publishProcessState {
		publishErr = publishReloadProviderScan(scan, publish)
	} else {
		publishErr = publish(cloneProviderScan(scan))
	}
	if publishErr != nil {
		var migrationRollbackErr error
		if ownershipChanged {
			migrationRollbackErr = s.rollbackProviderMigration()
		}
		rollbackErr := restoreConfigPreimages(preimages)
		return errors.Join(fmt.Errorf("publish reloaded configuration generation: %w", publishErr), migrationRollbackErr, rollbackErr)
	}

	// Rebuild staleness tracking. Track every discovered config path, not
	// just the ones that loaded, so a config file created after this reload
	// is detected as a change on the next staleness check.
	s.captureStalenessSnapshot(append(slices.Clone(configPaths), loadedPaths...))

	return nil
}
