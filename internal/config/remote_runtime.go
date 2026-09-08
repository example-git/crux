package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
)

var ErrRemoteRuntimeRevision = errors.New("client runtime revision changed")
var ErrClientRuntimeManaged = errors.New("this runtime is owned by the connected client; update or refresh it on that client and submit the next runtime revision")

// RegisterRemoteRuntimeSecrets runs only after admission selects client mode.
// Redaction outlives the workspace to protect delayed asynchronous log entries.
func (s *ConfigStore) RegisterRemoteRuntimeSecrets() {
	if s.RemoteAuthority() == nil {
		return
	}
	registerConfigSecrets(s.Config())
	redact.RegisterJSONValue(s.RuntimeSnapshot().clientRuntime.proposal.CredentialEnvironment)
	for _, binding := range s.RuntimeSnapshot().clientRuntime.proposal.Credentials {
		if binding.Account != nil {
			registerAccountSecrets(*binding.Account)
		}
	}
}

const (
	RemoteRuntimeVersion      = 1
	RemoteRuntimeCompiler     = "crux-declarative-runtime-v8"
	MaxRemoteRuntimeBytes     = 96 << 20
	MaxRemoteRuntimeBundles   = 64
	MaxRemoteRuntimeProviders = 64
)

// RemoteRuntimeProposal is private admission/update input. It must never be
// embedded in Workspace discovery, public events, logs or acknowledgements.
// Configurations contain resolved client values; only Credentials carries the
// provider's API/OAuth account token. Bundles retain original bytes and digests.
type RemoteRuntimeProposal struct {
	Version               int                                 `json:"version"`
	Revision              uint64                              `json:"revision"`
	Digest                string                              `json:"digest"`
	Bundles               []providerplugin.TransportBundle    `json:"bundles"`
	Providers             []RemoteProviderDefinition          `json:"providers"`
	Models                map[SelectedModelType]SelectedModel `json:"models"`
	Credentials           []RemoteCredentialBinding           `json:"credentials"`
	Images                *ImageConfiguration                 `json:"images,omitempty"`
	CredentialEnvironment map[string]string                   `json:"credential_environment,omitempty"`
}

type RemoteProviderDefinition struct {
	Config       ProviderConfig `json:"config"`
	BundleDigest string         `json:"bundle_digest,omitempty"`
}

type RemoteCredentialBinding struct {
	Owner       providerregistry.RegistrationOwner `json:"owner"`
	Generation  uint64                             `json:"generation"`
	APIKey      string                             `json:"api_key,omitempty"`
	Unavailable bool                               `json:"unavailable,omitempty"`
	Account     *accounts.Entry                    `json:"account,omitempty"`
}

// RemoteAuthority is the redacted acknowledgement, bound by the receiver to the
// authenticated principal. Credential values and private definitions are absent.
type RemoteAuthority struct {
	Mode      string                  `json:"mode"`
	Principal string                  `json:"principal"`
	Revision  uint64                  `json:"revision"`
	Digest    string                  `json:"digest"`
	Accounts  []RemoteAccountIdentity `json:"accounts"`
}

type RemoteAccountIdentity struct {
	ProviderID string `json:"provider_id"`
	AccountID  string `json:"account_id,omitempty"`
	Generation uint64 `json:"generation"`
}

type clientRuntimeState struct {
	authority RemoteAuthority
	proposal  RemoteRuntimeProposal
	bundles   map[string]providerplugin.DetachedBundle
}

// RemoteRuntimeDigest binds the entire private proposal, including exact
// account/generation/token choices. JSON map keys have deterministic ordering.
// Its value authenticates content only; it never grants principal authority.
func RemoteRuntimeDigest(proposal RemoteRuntimeProposal) (string, error) {
	proposal.Digest = ""
	data, err := json.Marshal(proposal)
	if err != nil {
		return "", errors.New("runtime proposal cannot be encoded")
	}
	if len(data) > MaxRemoteRuntimeBytes {
		return "", errors.New("runtime proposal exceeds receiver byte limit")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (s *ConfigStore) RemoteAuthority() *RemoteAuthority {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if s.clientRuntime == nil {
		return nil
	}
	authority := s.clientRuntime.authority
	authority.Accounts = slices.Clone(authority.Accounts)
	return &authority
}

func (s RuntimeSnapshot) IsClientOwned() bool { return s.clientRuntime != nil }

func (s RuntimeSnapshot) RemoteAuthority() *RemoteAuthority {
	if s.clientRuntime == nil {
		return nil
	}
	authority := s.clientRuntime.authority
	authority.Accounts = slices.Clone(authority.Accounts)
	return &authority
}

// ClientImageBundle distinguishes an absent client binding from server-owned
// mode. handled=true errors must never fall back to the host plugin manager.
func (s RuntimeSnapshot) ClientImageBundle(owner providerplugin.ImageOwner) (bundle providerplugin.RegisteredImageBundle, handled bool, err error) {
	if s.clientRuntime == nil {
		return bundle, false, nil
	}
	value, ok := s.clientRuntime.bundles[owner.Digest]
	if !ok || value.ID() != owner.PluginID || value.Version() != owner.Version || value.ProviderID() != owner.Backend {
		return bundle, true, errors.New("selected client image bundle is unavailable")
	}
	image := value.Image()
	if image == nil {
		return bundle, true, errors.New("selected client bundle is not an image provider")
	}
	return providerplugin.RegisteredImageBundle{Manifest: *image, Digest: value.Digest()}, true, nil
}

// ReplaceRemoteRuntime stages a complete candidate and publishes only after
// principal/revision checks and dependent runtime preparation succeed. Readers
// retain their prior captured Config, accounts, assets and revision unchanged.
func (s *ConfigStore) ReplaceRemoteRuntime(ctx context.Context, proposal RemoteRuntimeProposal, principal string, expectedRevision uint64) (*RemoteAuthority, error) {
	s.writeMu.RLock()
	if s.clientRuntime == nil || s.clientRuntime.authority.Principal != principal {
		s.writeMu.RUnlock()
		return nil, errors.New("runtime replacement requires its owning client principal")
	}
	workingDir, dataDir, debug := s.workingDir, s.config.Options.DataDirectory, s.config.Options.Debug
	baseEnvironment := cloneEnvironment(s.baseEnvironment)
	s.writeMu.RUnlock()
	candidate, err := CompileRemoteRuntime(workingDir, dataDir, debug, proposal, principal, baseEnvironment)
	if err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.clientRuntime == nil || s.clientRuntime.authority.Principal != principal {
		return nil, errors.New("client runtime ownership changed")
	}
	if s.clientRuntime.authority.Revision != expectedRevision || expectedRevision == ^uint64(0) || proposal.Revision != expectedRevision+1 {
		return nil, ErrRemoteRuntimeRevision
	}
	// Preserve workspace tool/UI settings while replacing all provider-owned
	// fields, without applying ordinary provider/model fallback or disk reload.
	next := s.Config().cloneForWrite()
	next.Providers = candidate.config.Providers
	next.Models = candidate.config.Models
	next.Images = candidate.config.Images
	next.bindProviderScan(*candidate.config.providerScan)
	next.captureExplicitModels()
	next.SetupAgents()
	candidate.config = next
	runtimeCandidate, err := s.prepareRuntimeGeneration(ctx, candidate.RuntimeSnapshot())
	if err != nil {
		return nil, fmt.Errorf("prepare client runtime replacement: %w", err)
	}
	committed := false
	defer func() {
		if !committed && runtimeCandidate.Abort != nil {
			runtimeCandidate.Abort()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	registerConfigSecrets(next)
	redact.RegisterJSONValue(candidate.clientRuntime.proposal.CredentialEnvironment)
	for _, account := range candidate.ephemeralAccounts {
		registerAccountSecrets(account.Entry)
	}
	s.configMu.Lock()
	s.config = next
	s.providerRegistry = candidate.providerRegistry
	s.knownProviders = candidate.knownProviders
	s.ephemeralAccounts = candidate.ephemeralAccounts
	s.clientRuntime = candidate.clientRuntime
	s.effectiveEnvironment = candidate.effectiveEnvironment
	s.resolver = candidate.resolver
	s.configMu.Unlock()
	if runtimeCandidate.Commit != nil {
		runtimeCandidate.Commit()
	}
	committed = true
	return candidate.RemoteAuthority(), nil
}

// CompileRemoteRuntime constructs a detached candidate without calling Load,
// a provider scan, migration, account-store access, or global publication. Its
// caller owns atomic admission/publication and must supply a verified principal.
func CompileRemoteRuntime(workingDir, dataDir string, debug bool, proposal RemoteRuntimeProposal, principal string, baseEnvironment env.Env) (*ConfigStore, error) {
	if proposal.Version != RemoteRuntimeVersion {
		return nil, errors.New("unsupported client runtime protocol version")
	}
	if proposal.Revision == 0 {
		return nil, errors.New("runtime revision must be positive")
	}
	if len(principal) != 64 {
		return nil, errors.New("verified client principal is required")
	}
	if _, err := hex.DecodeString(principal); err != nil {
		return nil, errors.New("invalid verified client principal")
	}
	if len(proposal.Bundles) > MaxRemoteRuntimeBundles || len(proposal.Providers) == 0 || len(proposal.Providers) > MaxRemoteRuntimeProviders || len(proposal.Credentials) > MaxRemoteRuntimeProviders {
		return nil, errors.New("runtime item count exceeds receiver limits")
	}
	digest, err := RemoteRuntimeDigest(proposal)
	if err != nil {
		return nil, err
	}
	if proposal.Digest != digest {
		return nil, errors.New("runtime snapshot digest mismatch")
	}
	// Own every mutable input before compilation; later caller mutations must
	// not alter accepted configuration, raw account metadata or selected models.
	encoded, err := json.Marshal(proposal)
	if err != nil {
		return nil, errors.New("runtime proposal cannot be copied")
	}
	var owned RemoteRuntimeProposal
	if json.Unmarshal(encoded, &owned) != nil {
		return nil, errors.New("runtime proposal cannot be decoded")
	}
	proposal = owned
	bundles := map[string]providerplugin.DetachedBundle{}
	bundleIDs := map[string]bool{}
	providerIDs := map[string]bool{}
	var totalBytes int64
	for _, input := range proposal.Bundles {
		for _, file := range input.Files {
			totalBytes += int64(len(file.Data))
			if totalBytes > providerplugin.MaxBundleBytes {
				return nil, errors.New("runtime bundle bytes exceed receiver limits")
			}
		}
		bundle, err := providerplugin.ValidateDetachedBundle(input)
		if err != nil {
			return nil, err
		}
		if bundleIDs[bundle.ID()] {
			return nil, errors.New("duplicate runtime bundle identity")
		}
		bundleIDs[bundle.ID()] = true
		// Image backend identities are a separate namespace from inference.
		key := bundle.ProviderID()
		if bundle.Type() == manifest.PluginTypeImageProvider {
			key = "image:" + key
		}
		if providerIDs[key] {
			return nil, errors.New("duplicate runtime provider identity")
		}
		providerIDs[key] = true
		bundles[bundle.Digest()] = bundle
	}
	providers := csync.NewMap[string, ProviderConfig]()
	scan := ProviderScan{presetReferences: map[string]ProviderPresetReference{}, pluginStatuses: map[string]providerplugin.Status{}, ownerModes: map[string]providerregistry.OwnerMode{}}
	var registrations []providerregistry.Registration
	usedBundles := map[string]bool{}
	for _, definition := range proposal.Providers {
		provider := definition.Config
		id := provider.ID
		if id == "" {
			return nil, errors.New("client provider ID is required")
		}
		if _, exists := providers.Get(id); exists {
			return nil, errors.New("duplicate client provider configuration")
		}
		if provider.APIKey != "" || provider.APIKeyTemplate != "" || provider.OAuthToken != nil {
			return nil, errors.New("provider tokens must use separate credential bindings")
		}
		if err := validateCompleteProviderOwner(id, provider); err != nil {
			return nil, errors.New("client provider owner reference is invalid")
		}
		endpoint, err := url.Parse(provider.BaseURL)
		if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http" && endpoint.Scheme != "wss" && endpoint.Scheme != "ws") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Fragment != "" {
			return nil, errors.New("client provider endpoint must be an explicit HTTP, HTTPS, or declared WebSocket destination")
		}
		var metadata catalog.Provider
		switch provider.Owner.Type {
		case ProviderOwnerPlugin, ProviderOwnerPreset:
			bundle, ok := bundles[definition.BundleDigest]
			if !ok || bundle.ProviderID() != id {
				return nil, errors.New("client provider requires its exact bundle digest")
			}
			usedBundles[bundle.Digest()] = true
			metadata, err = bundle.Catalog()
			if err != nil {
				return nil, err
			}
			if provider.Owner.Type == ProviderOwnerPlugin {
				full := bundle.Provider()
				if full == nil || provider.Plugin.ID != bundle.ID() || provider.Plugin.Version != bundle.Version() {
					return nil, errors.New("client plugin identity mismatch")
				}
				if isFullPluginReservedProviderID(id) {
					return nil, errors.New("client plugin claims a reserved core identity")
				}
				if _, _, migrated := providerplugin.MigratedProviderPreset(id); migrated {
					return nil, errors.New("client plugin claims a reserved preset identity")
				}
				registration, err := providerregistry.FromManifest(full.Manifest, full.StaticText)
				if err != nil {
					return nil, errors.New("client provider operations are incompatible with this host")
				}
				if !reflect.DeepEqual(provider.Owner, providerOwnerReferenceForRegistration(registration)) {
					return nil, errors.New("client plugin construction mismatch")
				}
				if registration.Operation != nil {
					if _, err := registration.Operation.ResolveEndpoint(provider.BaseURL); err != nil {
						return nil, errors.New("client provider endpoint violates its declared destination policy")
					}
				}
				if _, err := providerregistry.BindRegistrationConfiguration(registration, provider.Configuration); err != nil {
					return nil, errors.New("client provider configuration bindings are invalid")
				}
				registrations = append(registrations, registration)
				scan.pluginStatuses[bundle.ID()] = providerplugin.Status{ID: bundle.ID(), ProviderID: id, Version: bundle.Version(), Digest: bundle.Digest(), State: providerplugin.StateRegistered, Trust: providerplugin.TrustTrusted, Compatibility: providerplugin.CompatibilityCompatible}
				scan.ownerModes[id] = providerregistry.OwnerPluginNative
			} else {
				if bundle.Preset() == nil || provider.Preset.ID != bundle.ID() || provider.Preset.Version != bundle.Version() || provider.Preset.Digest != bundle.Digest() {
					return nil, errors.New("client preset identity mismatch")
				}
				if _, core := coreProviderConstruction(id); core {
					return nil, errors.New("client preset claims a reserved core identity")
				}
				if migratedProviderPresetMismatch(id, bundle.ID(), bundle.Version(), bundle.Digest()) {
					return nil, errors.New("client preset requires its canonical digest")
				}
				scan.presetReferences[id] = *provider.Preset
			}
		case ProviderOwnerCore:
			if definition.BundleDigest != "" {
				return nil, errors.New("core provider cannot bind a plugin bundle")
			}
			found := false
			for _, registration := range providerregistry.Integrated() {
				if registration.ProviderID == id && reflect.DeepEqual(provider.Owner, providerOwnerReferenceForRegistration(registration)) {
					registrations = append(registrations, registration)
					found = true
					break
				}
			}
			if !found {
				return nil, errors.New("client core construction is unavailable")
			}
		case ProviderOwnerCustom:
			if definition.BundleDigest != "" || provider.Owner.Construction != providerregistry.ConstructionOpenAICompat {
				return nil, errors.New("client custom provider construction is invalid")
			}
		default:
			return nil, errors.New("client provider authority is unsupported")
		}
		if endpoint.Scheme == "wss" || endpoint.Scheme == "ws" {
			if provider.Owner.Construction != providerregistry.ConstructionCodex || provider.Owner.Type == ProviderOwnerCore && endpoint.Scheme != "wss" {
				return nil, errors.New("client endpoint scheme does not match its selected transport")
			}
		}
		// The client has already resolved endpoint, header, key and model
		// defaults. Do not merge server or manifest defaults into these values.
		seenModels := map[string]bool{}
		for _, model := range provider.Models {
			if model.ID == "" || seenModels[model.ID] {
				return nil, errors.New("duplicate or empty client model ID")
			}
			seenModels[model.ID] = true
		}
		metadata.ID = catalog.ProviderID(id)
		metadata.Name = provider.Name
		metadata.Type = provider.Type
		metadata.APIEndpoint = provider.BaseURL
		metadata.Models = slices.Clone(provider.Models)
		scan.Providers = append(scan.Providers, metadata)
		providers.Set(id, provider)
	}
	for digest, bundle := range bundles {
		if !usedBundles[digest] && bundle.Type() != manifest.PluginTypeImageProvider {
			return nil, errors.New("runtime contains an unbound inference bundle")
		}
	}
	registry, err := providerregistry.New(registrations...)
	if err != nil {
		return nil, errors.New("client runtime registry has incompatible or conflicting owners")
	}
	scan.Registry = registry
	cfg := &Config{Providers: providers, Models: maps.Clone(proposal.Models), Images: proposal.Images}
	if baseEnvironment == nil {
		baseEnvironment = env.NewFromMap(map[string]string{})
	}
	if err := cfg.setDefaultsFromEnvironment(workingDir, dataDir, baseEnvironment); err != nil {
		return nil, errors.New("client runtime defaults are invalid")
	}
	cfg.Tools.CodebaseSearch.StoreDirectory = filepath.Join(cfg.Options.DataDirectory, "codebase-index")
	cfg.Options.Debug = debug
	cfg.captureExplicitModels()
	cfg.bindProviderScan(scan)
	for id, provider := range providers.Seq2() {
		if _, ok := providerOwnerForProvider(cfg, registry, id, provider); !ok {
			return nil, errors.New("client provider owner cannot be activated")
		}
		if err := cfg.ValidateProviderConfiguration(id, provider.Configuration); err != nil {
			return nil, errors.New("client provider configuration violates its schema")
		}
	}
	authority := RemoteAuthority{Mode: "client", Principal: principal, Revision: proposal.Revision, Digest: digest}
	forwarded := map[string]ForwardedAccount{}
	credentialProviders := map[string]bool{}
	unavailable := map[string]bool{}
	for _, binding := range proposal.Credentials {
		id := binding.Owner.ProviderID
		provider, ok := providers.Get(id)
		if !ok || credentialProviders[id] || binding.Generation == 0 {
			return nil, errors.New("invalid or duplicate client credential binding")
		}
		expected, ok := providerOwnerForProvider(cfg, registry, id, provider)
		if !ok || expected != binding.Owner {
			return nil, errors.New("client credential does not match its exact provider owner")
		}
		credentialProviders[id] = true
		if binding.Unavailable && (binding.APIKey != "" || binding.Account != nil) {
			return nil, errors.New("unavailable client credential cannot contain a secret")
		}
		unavailable[id] = binding.Unavailable
		identity := RemoteAccountIdentity{ProviderID: id, Generation: binding.Generation}
		if binding.Account != nil {
			registration, ok := cfg.ProviderRegistration(id)
			if !ok || registration.OAuth == nil || binding.Account.ID == "" || binding.Account.AccessToken == "" || binding.APIKey != "" {
				return nil, errors.New("invalid client OAuth account binding")
			}
			provider.OAuthToken = binding.Account.Token()
			provider.APIKey = binding.Account.AccessToken
			identity.AccountID = binding.Account.ID
			forwarded[expected.AccountNamespace] = ForwardedAccount{Owner: expected, Entry: *binding.Account}
		} else {
			provider.APIKey = binding.APIKey
		}
		provider.APIKeyTemplate = provider.APIKey
		providers.Set(id, provider)
		authority.Accounts = append(authority.Accounts, identity)
	}
	if err := validateForwardedAccounts(cfg, forwarded); err != nil {
		return nil, errors.New("client accounts cannot bind to the runtime")
	}
	if err := cfg.Images.Validate(); err != nil {
		return nil, errors.New("client image configuration is invalid")
	}
	declaredEnvironment := map[string]bool{}
	if cfg.Images != nil {
		checkImage := func(owner providerplugin.ImageOwner) error {
			bundle, ok := bundles[owner.Digest]
			if !ok || bundle.Type() != manifest.PluginTypeImageProvider || bundle.ID() != owner.PluginID || bundle.Version() != owner.Version || bundle.ProviderID() != owner.Backend {
				return errors.New("client image selection requires its exact received bundle")
			}
			for _, credential := range bundle.Image().Credentials {
				if credential.Source == "environment" {
					declaredEnvironment[credential.Environment] = true
					if proposal.CredentialEnvironment[credential.Environment] == "" {
						return errors.New("selected client image environment credential is missing")
					}
				}
			}
			return nil
		}
		for _, owner := range cfg.Images.Preferred {
			if err := checkImage(owner); err != nil {
				return nil, err
			}
		}
		for _, image := range cfg.Images.Providers {
			if err := checkImage(image.Owner); err != nil {
				return nil, err
			}
			for _, owner := range image.Credentials {
				actual, ok := providerOwnerForConfig(cfg, registry, owner.ProviderID)
				if !ok || actual != owner || !credentialProviders[owner.ProviderID] {
					return nil, errors.New("client image credential requires its exact received provider binding")
				}
			}
		}
	}
	if len(proposal.CredentialEnvironment) > 256 {
		return nil, errors.New("too many client image environment credentials")
	}
	for name, value := range proposal.CredentialEnvironment {
		if !declaredEnvironment[name] || len(value) > 1<<20 {
			return nil, errors.New("undeclared or oversized client image environment credential")
		}
	}
	if len(cfg.Models) == 0 {
		return nil, errors.New("client selected models are required")
	}
	for kind, selected := range cfg.Models {
		if kind != SelectedModelTypeLarge && kind != SelectedModelTypeSmall {
			return nil, errors.New("unsupported client model role")
		}
		provider, ok := providers.Get(selected.Provider)
		if !ok {
			return nil, fmt.Errorf("selected %s client provider is missing", kind)
		}
		if !slices.ContainsFunc(provider.Models, func(model catalog.Model) bool { return model.ID == selected.Model }) {
			return nil, fmt.Errorf("selected %s client model is unavailable", kind)
		}
		if unavailable[selected.Provider] || provider.Disable {
			continue
		}
		registration, registered := cfg.ProviderRegistration(selected.Provider)
		if registered && registration.OAuth != nil && provider.OAuthToken == nil {
			return nil, fmt.Errorf("selected %s client provider requires its selected OAuth account", kind)
		}
		requiresAPI := !registered || registration.Manifest == nil
		if registered && registration.Manifest != nil {
			for _, credential := range registration.Manifest.Capabilities.Credentials {
				if credential.Kind != "none" && credential.ConfigProperty == "" {
					requiresAPI = true
				}
			}
		}
		if requiresAPI && provider.APIKey == "" {
			return nil, fmt.Errorf("selected %s client provider requires its client credential", kind)
		}
	}
	cfg.SetupAgents()
	return &ConfigStore{config: cfg, workingDir: workingDir, baseEnvironment: cloneEnvironment(baseEnvironment), effectiveEnvironment: env.NewFromMap(maps.Clone(proposal.CredentialEnvironment)), resolver: IdentityResolver(), providerRegistry: registry, knownProviders: cloneProviderCatalog(scan.Providers), ephemeralAccounts: forwarded, clientRuntime: &clientRuntimeState{authority: authority, proposal: proposal, bundles: bundles}}, nil
}

// ClientProviderUnavailable returns an explicit accepted client availability
// state. A missing unmarked binding remains an admission error.
func (s RuntimeSnapshot) ClientProviderUnavailable(id string) error {
	if !s.IsClientOwned() {
		return nil
	}
	if provider, ok := s.config.Providers.Get(id); ok && provider.Disable {
		return fmt.Errorf("client provider %s is disabled", id)
	}
	for _, credential := range s.clientRuntime.proposal.Credentials {
		if credential.Owner.ProviderID == id && credential.Unavailable {
			return fmt.Errorf("client provider %s has no credential; sign in on the owning client", id)
		}
	}
	return nil
}
