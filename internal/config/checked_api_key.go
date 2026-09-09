package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
)

// CheckedAPIKeyPreparation retains private Check evidence and its exact input
// observation. It is neither a configuration nor a transferable credential.
type CheckedAPIKeyPreparation struct {
	before   AuthenticationCapture
	owner    providerregistry.RegistrationOwner
	provider ProviderConfig
	settings checkedAPIKeySettings
	layers   authenticationLayers
	probe    ConnectionProbeResult
	valid    bool
}

func (CheckedAPIKeyPreparation) MarshalJSON() ([]byte, error) {
	return nil, errors.New("checked API key preparations are private")
}
func (CheckedAPIKeyPreparation) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private checked API key preparation]"))
}
func (p CheckedAPIKeyPreparation) ProbeResult() ConnectionProbeResult {
	if p.probe.Kind == "" {
		return ConnectionProbeResult{Kind: ConnectionProbeNotProbed, Policy: ConnectionProbePolicyNone}
	}
	return p.probe
}

type checkedAPIKeySettings struct {
	base                                  env.Env
	ephemeral                             map[string]ProviderConfig
	overrides                             RuntimeOverrides
	globalPath, workspacePath, workingDir string
}

func (checkedAPIKeySettings) MarshalJSON() ([]byte, error) {
	return nil, errors.New("checked API key settings are private")
}
func (checkedAPIKeySettings) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private checked API key settings]"))
}
func (s *ConfigStore) checkedAPIKeySettingsLocked(before AuthenticationCapture) checkedAPIKeySettings {
	base := s.baseEnvironment
	if base == nil {
		base = before.runtime.environment
	}
	settings := checkedAPIKeySettings{base: cloneEnvironment(base), ephemeral: map[string]ProviderConfig{}, overrides: cloneRuntimeOverrides(s.overrides), globalPath: s.globalDataPath, workspacePath: s.workspacePath, workingDir: s.workingDir}
	for id, p := range s.ephemeralProviderConfigs {
		settings.ephemeral[id] = cloneProviderConfig(p)
	}
	return settings
}
func (s *ConfigStore) validateCheckedAPIKeyCapture(ctx context.Context, before AuthenticationCapture, owner providerregistry.RegistrationOwner) error {
	if before.runtime.IsClientOwned() {
		return ErrClientRuntimeManaged
	}
	if before.runtime.publicationStore != s || !before.inputs.valid {
		return errors.New("checked API key requires its original owning store capture")
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return err
	}
	defer s.writeMu.RUnlock()
	if err := validateAuthenticationCandidateOwner(before, owner); err != nil {
		return err
	}
	return s.verifyAuthenticationCollectionLocked(ctx, before)
}

// PrepareCheckedAPIKey resolves input once and probes the exact resulting
// provider. Save consumes this retained value; it never repeats resolution or
// probing, and it cannot replace this capture with a later account observation.
func (s *ConfigStore) PrepareCheckedAPIKey(ctx context.Context, before AuthenticationCapture, owner providerregistry.RegistrationOwner, credentialID, source string) (prepared CheckedAPIKeyPreparation, err error) {
	prepared.probe = prepared.ProbeResult()
	if err := s.validateCheckedAPIKeyCapture(ctx, before, owner); err != nil {
		return prepared, err
	}
	if credentialID != "provider.api_key" {
		return prepared, errProviderAPIKeySlotUnsupported
	}
	if source == "" {
		return prepared, errors.New("API key source is empty")
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return prepared, err
	}
	settings := s.checkedAPIKeySettingsLocked(before)
	s.writeMu.RUnlock()
	layers, err := before.checkedAPIKeyLayers(settings)
	if err != nil {
		return prepared, err
	}
	if err := before.validateConfigBasis(layers, ""); err != nil {
		return prepared, err
	}
	provider, configured, err := checkedAPIKeyProviderTarget(before, owner)
	if err != nil {
		return prepared, err
	}
	// A compatible catalog type is not a native operation probe policy.
	// Custom/preset identities omit Construction in RegistrationOwner; the
	// complete provider reference was just validated against that exact owner.
	if provider.Owner.Construction != providerregistry.ConstructionOpenAICompat {
		prepared.probe = ConnectionProbeResult{Kind: ConnectionProbeUnsupported, Policy: ConnectionProbePolicyNone}
		return prepared, errors.New("checked API key connection policy is not implemented for this provider construction")
	}
	resolver, ok := before.runtime.resolver.(contextVariableResolver)
	if !ok {
		return prepared, errors.New("checked API key resolver does not support cancellation")
	}
	resolve := func(value string) (string, error) {
		value, err := resolver.ResolveValueContext(ctx, value)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			return "", errors.New("checked provider input could not be resolved")
		}
		return value, nil
	}
	literal, err := resolve(source)
	if err != nil {
		return prepared, err
	}
	if literal == "" {
		return prepared, errors.New("resolved API key is empty")
	}
	provider, err = bindResolvedProviderAPIKey(before.runtime, provider, owner, source, literal)
	if err != nil {
		return prepared, err
	}
	endpointSource := provider.BaseURL
	if provider.resolvedEndpoint != nil {
		if err := before.runtime.validateResolvedProviderEndpointOwner(provider); err != nil {
			return prepared, err
		}
		endpointSource = provider.resolvedEndpoint.source
	}
	endpoint, err := ResolveProviderEndpoint(provider, resolve)
	if err != nil {
		return prepared, err
	}
	provider, err = bindResolvedProviderEndpoint(before.runtime, provider, owner, endpointSource, endpoint)
	if err != nil {
		return prepared, err
	}
	if !configured {
		for _, name := range slices.Sorted(maps.Keys(provider.ExtraHeaders)) {
			value, err := resolve(provider.ExtraHeaders[name])
			if err != nil {
				return prepared, err
			}
			if value == "" {
				delete(provider.ExtraHeaders, name)
			} else {
				provider.ExtraHeaders[name] = value
			}
		}
	}
	validate := func() error { return s.validateCheckedAPIKeyCapture(ctx, before, owner) }
	prepared.probe, err = provider.ProbeConnection(ctx, IdentityResolver(), validate)
	if err != nil {
		if ctx.Err() != nil {
			return prepared, ctx.Err()
		}
		return prepared, errors.New("checked provider connection failed")
	}
	if err := validate(); err != nil {
		return prepared, err
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return prepared, err
	}
	same := reflect.DeepEqual(settings, s.checkedAPIKeySettingsLocked(before))
	s.writeMu.RUnlock()
	if !same {
		return prepared, errors.New("checked provider settings changed")
	}
	prepared.before, prepared.owner, prepared.provider, prepared.layers, prepared.settings, prepared.valid = before, owner, provider, layers, settings, true
	return prepared, nil
}

func checkedAPIKeyProviderTarget(before AuthenticationCapture, owner providerregistry.RegistrationOwner) (ProviderConfig, bool, error) {
	if err := validateAuthenticationCandidateOwner(before, owner); err != nil {
		return ProviderConfig{}, false, err
	}
	provider, configured := ProviderConfig{}, false
	if before.runtime.config.Providers != nil {
		provider, configured = before.runtime.config.Providers.Get(owner.ProviderID)
	}
	provider = cloneProviderConfig(provider)
	if configured {
		var err error
		provider, err = prepareConfiguredProviderOwner(owner.ProviderID, provider)
		if err != nil {
			return ProviderConfig{}, false, errors.New("checked provider ownership is invalid")
		}
		provider = before.runtime.config.completeProviderOwner(owner.ProviderID, provider)
	} else {
		registration, _ := before.runtime.registry.Lookup(owner.ProviderID)
		var err error
		provider, err = authenticationUnconfiguredProvider(before, owner, registration)
		if err != nil {
			return ProviderConfig{}, false, err
		}
	}
	actual, active := before.runtime.ProviderOwnerFor(owner.ProviderID, provider)
	if !active || actual != owner || !providerAPIKeySlotSupported(before.runtime, provider) {
		return ProviderConfig{}, false, errProviderAPIKeySlotUnsupported
	}
	scratch := before.runtime.config.cloneForWrite()
	if scratch.Providers == nil {
		scratch.Providers = csync.NewMap[string, ProviderConfig]()
	}
	scratch.Providers.Set(owner.ProviderID, provider)
	if err := scratch.ValidateProviderConfiguration(owner.ProviderID, provider.Configuration); err != nil {
		return ProviderConfig{}, false, errors.New("checked provider configuration is invalid")
	}
	return provider, configured, nil
}

// Reuse accepted evaluated shell sources: Check executes only the explicit
// credential/endpoint/header inputs, never unrelated configuration programs.
func (before AuthenticationCapture) checkedAPIKeyLayers(settings checkedAPIKeySettings) (authenticationLayers, error) {
	layers, err := before.authenticationReconciliationLayers()
	if err != nil {
		return layers, err
	}
	if len(settings.ephemeral) > 0 {
		data, err := json.Marshal(struct {
			Providers map[string]ProviderConfig `json:"providers"`
		}{settings.ephemeral})
		if err != nil {
			return layers, errors.New("checked provider overlay cannot be encoded")
		}
		layers.overlays = append(layers.overlays, data)
	}
	if len(settings.overrides.Models) > 0 {
		layers.pinned = make(map[SelectedModelType]SelectedModel, len(settings.overrides.Models))
		for kind, model := range settings.overrides.Models {
			layers.pinned[kind] = cloneSelectedModel(model)
		}
	}
	return layers, nil
}
