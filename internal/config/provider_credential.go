package config

import (
	"errors"
	"fmt"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/discover"
	"github.com/example-git/crux/internal/providerregistry"
)

var (
	errResolvedProviderAPIKeyStale   = errors.New("resolved provider credential no longer matches its owner or source")
	errProviderAPIKeySlotUnsupported = errors.New("provider does not support the provider.api_key credential slot")
)

// This proves literal-byte provenance, not a successful connection probe.
// Only a retained check receipt can prove what definition and policy was probed.
type resolvedProviderAPIKey struct {
	owner           providerregistry.RegistrationOwner
	slot            string
	source, literal string
	references      ProviderConfig
}

func (resolvedProviderAPIKey) MarshalJSON() ([]byte, error) {
	return nil, errors.New("resolved provider credentials are private")
}

func (resolvedProviderAPIKey) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private resolved provider credential]"))
}

func (binding *resolvedProviderAPIKey) matches(provider ProviderConfig) bool {
	return binding != nil && binding.slot == "provider.api_key" &&
		provider.ID == binding.owner.ProviderID && provider.APIKey == binding.literal &&
		provider.APIKeyTemplate == binding.source && provider.OAuthToken == nil &&
		providerOwnershipReferencesMatch(provider, binding.references)
}

// bindResolvedProviderAPIKey is a pure preparation step. It neither resolves
// input nor verifies connectivity nor publishes/writes any state.
func bindResolvedProviderAPIKey(snapshot RuntimeSnapshot, provider ProviderConfig, owner providerregistry.RegistrationOwner, source, literal string) (ProviderConfig, error) {
	actual, active := snapshot.ProviderOwnerFor(provider.ID, provider)
	current, currentActive := snapshot.ProviderOwner(provider.ID)
	if !active || !currentActive || actual != owner || current != owner || source == "" || literal == "" {
		return ProviderConfig{}, errResolvedProviderAPIKeyStale
	}
	if !providerAPIKeySlotSupported(snapshot, provider) {
		return ProviderConfig{}, errProviderAPIKeySlotUnsupported
	}
	provider = cloneProviderConfig(provider)
	provider.APIKey, provider.APIKeyTemplate, provider.OAuthToken = literal, source, nil
	provider.resolvedAPIKey = &resolvedProviderAPIKey{
		owner: owner, slot: "provider.api_key", source: source, literal: literal,
		references: ProviderConfig{ID: provider.ID, Owner: clonePointer(provider.Owner), Plugin: clonePointer(provider.Plugin), Preset: clonePointer(provider.Preset)},
	}
	return provider, nil
}

func providerAPIKeySlotSupported(snapshot RuntimeSnapshot, provider ProviderConfig) bool {
	if provider.Owner == nil {
		return false
	}
	if provider.Owner.Type == ProviderOwnerCustom || provider.Owner.Type == ProviderOwnerPreset {
		return provider.Type == "" || provider.Type == catalog.TypeOpenAICompat || discover.IsKnownCustomProvider(string(provider.Type))
	}
	registration, active := providerDeclaredRegistrationForProvider(snapshot.registry, provider.ID, provider)
	return active && registrationAPIKeySlotSupported(registration)
}

// This follows the constructor's existing declaration-to-provider.api_key
// mapping. It establishes slot support, not proof of use in a request.
// Configuration properties use a different slot; endpoint credentials are
// optional because headers and request templates can also read this value.
func registrationAPIKeySlotSupported(registration providerregistry.Registration) bool {
	switch registration.Construction {
	case providerregistry.ConstructionOpenAIResponses, providerregistry.ConstructionAnthropicMessages,
		providerregistry.ConstructionGeminiContent, providerregistry.ConstructionGeminiInteraction, providerregistry.ConstructionGenericJSON:
	default:
		return false
	}
	if registration.Manifest == nil {
		return false
	}
	for _, credential := range registration.Manifest.Capabilities.Credentials {
		if credential.ConfigProperty == "" && (credential.Kind == "api-key" || credential.Kind == "bearer") {
			return true
		}
	}
	return false
}

// providerAPIKeySourceProjection restores only the authored credential source
// for fixed persistence/input-basis comparison. It is not an executable provider.
func providerAPIKeySourceProjection(provider ProviderConfig) (ProviderConfig, error) {
	if provider.resolvedAPIKey == nil {
		return cloneProviderConfig(provider), nil
	}
	if !provider.resolvedAPIKey.matches(provider) {
		return ProviderConfig{}, errResolvedProviderAPIKeyStale
	}
	provider = cloneProviderConfig(provider)
	provider.APIKey = provider.resolvedAPIKey.source
	provider.APIKeyTemplate, provider.resolvedAPIKey = "", nil
	return provider, nil
}

// ResolveProviderAPIKey preserves resolved API keys and proven OAuth access
// tokens byte-for-byte. Ordinary source expressions retain resolver semantics.
// Snapshot consumers additionally validate the complete private owner below.
func ResolveProviderAPIKey(provider ProviderConfig, resolve func(string) (string, error)) (string, error) {
	if provider.resolvedAPIKey != nil {
		if !provider.resolvedAPIKey.matches(provider) {
			return "", errResolvedProviderAPIKeyStale
		}
		return provider.resolvedAPIKey.literal, nil
	}
	if providerHasLiteralOAuthCredential(provider) {
		return provider.APIKey, nil
	}
	return resolve(provider.APIKey)
}

func (s RuntimeSnapshot) validateResolvedProviderAPIKeyOwner(provider ProviderConfig) error {
	if err := s.validateResolvedConfigurationCredentials(provider); err != nil {
		return err
	}
	if provider.resolvedAPIKey == nil {
		return nil
	}
	owner, active := s.ProviderOwnerFor(provider.ID, provider)
	current, currentActive := s.ProviderOwner(provider.ID)
	if !active || !currentActive || owner != provider.resolvedAPIKey.owner || current != owner || !provider.resolvedAPIKey.matches(provider) {
		return errResolvedProviderAPIKeyStale
	}
	if s.config.Providers == nil {
		return errResolvedProviderAPIKeyStale
	}
	persisted, present := s.config.Providers.Get(provider.ID)
	if !present || !provider.resolvedAPIKey.matches(persisted) ||
		!persisted.resolvedAPIKey.matches(persisted) || persisted.resolvedAPIKey.owner != owner {
		return errResolvedProviderAPIKeyStale
	}
	return nil
}

// UsesResolvedProviderAPIKey identifies an explicitly resolved API-key slot,
// not OAuth access-token bytes or evidence of a successful connection probe.
// Detached client configurations are already compiled from a selected literal
// credential binding and use an identity resolver.
func (s RuntimeSnapshot) UsesResolvedProviderAPIKey(providerID string) (bool, error) {
	if s.config == nil || s.config.Providers == nil {
		return false, nil
	}
	provider, exists := s.config.Providers.Get(providerID)
	if !exists {
		return false, nil
	}
	if provider.resolvedAPIKey != nil {
		if err := s.validateResolvedProviderAPIKeyOwner(provider); err != nil {
			return false, err
		}
		return true, nil
	}
	return s.IsClientOwned() && provider.OAuthToken == nil, nil
}

func (s RuntimeSnapshot) ResolveProviderAPIKey(provider ProviderConfig) (string, error) {
	if err := s.RuntimeRevocation(); err != nil {
		return "", err
	}
	if err := s.validateResolvedProviderAPIKeyOwner(provider); err != nil {
		return "", err
	}
	return ResolveProviderAPIKey(provider, s.Resolve)
}

func providerHasLiteralAPIKey(provider ProviderConfig) bool {
	return provider.resolvedAPIKey.matches(provider) || providerHasLiteralOAuthCredential(provider)
}

func providerHasLiteralOAuthCredential(provider ProviderConfig) bool {
	return provider.OAuthToken != nil && provider.APIKey == provider.OAuthToken.AccessToken
}
