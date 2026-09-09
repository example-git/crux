package config

import (
	"errors"
	"fmt"
	"slices"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
)

// ProviderCredentialSlot contains declaration/status only, never a credential
// value or expression. Configuration IDs name exact manifest credential IDs.
type ProviderCredentialSlot struct {
	ID, Kind, Property string
	Configured         bool
}

type resolvedProviderConfigurationCredential struct {
	owner                           providerregistry.RegistrationOwner
	slot, property, source, literal string
	references                      ProviderConfig
}

func (resolvedProviderConfigurationCredential) MarshalJSON() ([]byte, error) {
	return nil, errors.New("resolved configuration credentials are private")
}
func (resolvedProviderConfigurationCredential) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private resolved configuration credential]"))
}

var errResolvedConfigurationCredential = errors.New("resolved configuration credential no longer matches its exact owner, slot or source")

func (b *resolvedProviderConfigurationCredential) matches(provider ProviderConfig) bool {
	if b == nil {
		return false
	}
	value, ok := provider.Configuration[b.property].(string)
	return ok && provider.ID == b.owner.ProviderID && value == b.literal && providerOwnershipReferencesMatch(provider, b.references)
}

func (capture AuthenticationCapture) CredentialSlots(owner providerregistry.RegistrationOwner) []ProviderCredentialSlot {
	if !slices.Contains(capture.owners, owner) {
		return nil
	}
	provider, configured := capture.runtime.config.authenticationCollectionProvider(owner.ProviderID)
	if !configured {
		registration, ok := capture.runtime.registry.Lookup(owner.ProviderID)
		if owner.HasPreset {
			provider.Owner = providerPresetOwnerReference()
			provider.Preset = &ProviderPresetReference{ID: owner.PresetID, Version: owner.PresetVersion, Digest: owner.PresetDigest}
		} else if !ok || registration.Owner() != owner {
			return nil
		} else {
			provider.Owner = providerOwnerReferenceForRegistration(registration)
			if registration.Manifest != nil {
				provider.Plugin = &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}
			}
		}
	}
	provider.ID = owner.ProviderID
	return providerCredentialSlots(capture.runtime, provider)
}

func providerCredentialSlots(snapshot RuntimeSnapshot, provider ProviderConfig) []ProviderCredentialSlot {
	result := []ProviderCredentialSlot{}
	if providerAPIKeySlotSupported(snapshot, provider) {
		result = append(result, ProviderCredentialSlot{ID: "provider.api_key", Kind: "api-key", Configured: provider.APIKey != "" || provider.APIKeyTemplate != ""})
	}
	registration, ok := providerDeclaredRegistrationForProvider(snapshot.registry, provider.ID, provider)
	if !ok || registration.Manifest == nil {
		return result
	}
	for _, declaration := range registration.Manifest.Capabilities.Credentials {
		if declaration.ConfigProperty == "" || declaration.Kind == "none" {
			continue
		}
		value, present := provider.Configuration[declaration.ConfigProperty].(string)
		result = append(result, ProviderCredentialSlot{ID: "configuration." + declaration.ID, Kind: declaration.Kind, Property: declaration.ConfigProperty, Configured: present && value != ""})
	}
	return result
}

func providerCredentialSlot(snapshot RuntimeSnapshot, provider ProviderConfig, id string) (ProviderCredentialSlot, error) {
	for _, slot := range providerCredentialSlots(snapshot, provider) {
		if slot.ID == id {
			return slot, nil
		}
	}
	return ProviderCredentialSlot{}, errors.New("provider does not declare the selected credential slot")
}

func providerMissingConfigurationCredentials(snapshot RuntimeSnapshot, provider ProviderConfig) bool {
	for _, slot := range providerCredentialSlots(snapshot, provider) {
		if slot.Property == "" {
			continue
		}
		value, present := provider.Configuration[slot.Property]
		text, isString := value.(string)
		if !present || isString && text == "" {
			return true
		}
	}
	return false
}

// ProviderCredentialSetupPending retains selected models during partial setup.
// It permits preparing a visibly unavailable runtime, never provider requests.
func (snapshot RuntimeSnapshot) ProviderCredentialSetupPending(providerID string) error {
	if snapshot.config == nil {
		return nil
	}
	provider, exists := snapshot.config.authenticationCollectionProvider(providerID)
	if !exists {
		return nil
	}
	if _, active := snapshot.ProviderOwnerFor(providerID, provider); !active || !providerMissingConfigurationCredentials(snapshot, provider) {
		return nil
	}
	return fmt.Errorf("provider %q requires additional configuration credentials", providerID)
}

func providerNeedsPrimaryCredential(registration providerregistry.Registration) bool {
	if registration.Manifest == nil {
		return true
	}
	for _, credential := range registration.Manifest.Capabilities.Credentials {
		if credential.Kind != "none" && credential.ConfigProperty == "" {
			return true
		}
	}
	return false
}

func bindResolvedConfigurationCredential(snapshot RuntimeSnapshot, provider ProviderConfig, owner providerregistry.RegistrationOwner, slot ProviderCredentialSlot, source, literal string) (ProviderConfig, error) {
	actual, ok := snapshot.ProviderOwnerFor(provider.ID, provider)
	current, currentActive := snapshot.ProviderOwner(provider.ID)
	declared, err := providerCredentialSlot(snapshot, provider, slot.ID)
	if !ok || !currentActive || actual != owner || current != owner || err != nil || declared.Property == "" || declared.Property != slot.Property || source == "" || literal == "" {
		return ProviderConfig{}, errResolvedConfigurationCredential
	}
	provider = cloneProviderConfig(provider)
	if provider.Configuration == nil {
		provider.Configuration = make(map[string]any)
	}
	if provider.resolvedCredentials == nil {
		provider.resolvedCredentials = make(map[string]*resolvedProviderConfigurationCredential)
	}
	// Several declarations may intentionally reference the same property. Its
	// replacement retires their old proofs; it never changes a different key.
	for id, binding := range provider.resolvedCredentials {
		if binding != nil && binding.property == slot.Property {
			delete(provider.resolvedCredentials, id)
		}
	}
	provider.Configuration[slot.Property] = literal
	provider.resolvedCredentials[slot.ID] = &resolvedProviderConfigurationCredential{owner: owner, slot: slot.ID, property: slot.Property, source: source, literal: literal, references: ProviderConfig{ID: provider.ID, Owner: clonePointer(provider.Owner), Plugin: clonePointer(provider.Plugin), Preset: clonePointer(provider.Preset)}}
	redact.Register(source, literal)
	return provider, nil
}

func (snapshot RuntimeSnapshot) validateResolvedConfigurationCredentials(provider ProviderConfig) error {
	if len(provider.resolvedCredentials) == 0 {
		return nil
	}
	owner, ok := snapshot.ProviderOwnerFor(provider.ID, provider)
	if !ok {
		return errResolvedConfigurationCredential
	}
	current, active := snapshot.ProviderOwner(provider.ID)
	if !active || current != owner || snapshot.config == nil {
		return errResolvedConfigurationCredential
	}
	persisted, present := snapshot.config.authenticationCollectionProvider(provider.ID)
	if !present {
		return errResolvedConfigurationCredential
	}
	for id, binding := range provider.resolvedCredentials {
		slot, err := providerCredentialSlot(snapshot, provider, id)
		if binding == nil || binding.owner != owner || binding.slot != id || !binding.matches(provider) || err != nil || slot.Property != binding.property {
			return errResolvedConfigurationCredential
		}
		retained := persisted.resolvedCredentials[id]
		if !binding.matches(persisted) || !retained.matches(persisted) || retained.owner != owner || retained.slot != id || retained.source != binding.source || retained.property != binding.property {
			return errResolvedConfigurationCredential
		}
	}
	return nil
}

// Only declared configuration credentials are expressions. Ordinary plugin
// configuration strings retain their schema-defined literal semantics.
func resolveProviderConfigurationCredentials(snapshot RuntimeSnapshot, provider ProviderConfig, resolve func(string) (string, error)) (ProviderConfig, error) {
	if err := snapshot.validateResolvedConfigurationCredentials(provider); err != nil {
		return ProviderConfig{}, err
	}
	owner, ok := snapshot.ProviderOwnerFor(provider.ID, provider)
	if !ok {
		return provider, nil
	}
	for _, slot := range providerCredentialSlots(snapshot, provider) {
		if slot.Property == "" {
			continue
		}
		retained := false
		for _, binding := range provider.resolvedCredentials {
			if binding.property == slot.Property {
				retained = true
				break
			}
		}
		if retained {
			continue
		}
		source, present := provider.Configuration[slot.Property].(string)
		if !present || source == "" {
			continue
		}
		literal, err := resolve(source)
		if err != nil {
			return ProviderConfig{}, errors.New("declared provider credential source could not be resolved")
		}
		if literal == "" {
			return ProviderConfig{}, errors.New("declared provider credential resolved to an empty value")
		}
		provider, err = bindResolvedConfigurationCredential(snapshot, provider, owner, slot, source, literal)
		if err != nil {
			return ProviderConfig{}, err
		}
	}
	return provider, nil
}

func checkedProviderCredentialMatches(provider ProviderConfig, slot ProviderCredentialSlot) bool {
	if slot.ID == "provider.api_key" {
		return provider.resolvedAPIKey.matches(provider)
	}
	return slot.Property != "" && provider.resolvedCredentials[slot.ID].matches(provider)
}

// Property saves preserve the exact construction account selected by the
// captured runtime; the primary API-key transaction retains its old behavior.
func (capture AuthenticationCapture) finalizeRuntimeConfigurationCredential(candidate *Config, owner providerregistry.RegistrationOwner, slot ProviderCredentialSlot) error {
	if candidate == nil || candidate == capture.runtime.config || capture.runtime.IsClientOwned() {
		return errResolvedConfigurationCredential
	}
	provider, ok := candidate.Providers.Get(owner.ProviderID)
	if !ok || !checkedProviderCredentialMatches(provider, slot) {
		return errResolvedConfigurationCredential
	}
	retained := &authenticationRuntimeAccounts{target: owner, entries: make(map[providerregistry.RegistrationOwner]*accounts.Entry)}
	if previous := capture.runtime.config.authenticationAccounts; previous != nil {
		retained.target = previous.target
	}
	for _, current := range capture.owners {
		if current.AccountNamespace == "" {
			continue
		}
		if actual, ok := candidate.ProviderOwner(current.ProviderID); !ok || actual != current {
			return errResolvedConfigurationCredential
		}
		entry, captured, err := capture.runtime.CapturedConstructionAccount(current)
		if err != nil {
			return err
		}
		if !captured {
			for _, stored := range capture.accounts.Entries(current.AccountNamespace) {
				if stored.ID == capture.accounts.ActiveID(current.AccountNamespace) {
					entry = cloneConstructionAccount(&stored)
					break
				}
			}
		}
		retained.entries[current] = cloneConstructionAccount(entry)
	}
	candidate.authenticationAccounts = retained
	return nil
}
