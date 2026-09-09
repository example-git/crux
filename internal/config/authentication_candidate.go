package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
)

// prepareAuthenticationProvider prepares only the captured target. It neither
// refreshes accounts nor loads/scans/publishes configuration. The transaction
// must revalidate its capture after this cancellable header work and before
// preparing runtime or acquiring its final account/config-file leases.
func prepareAuthenticationProvider(ctx context.Context, before AuthenticationCapture, owner providerregistry.RegistrationOwner, token *oauth.Token) (ProviderConfig, error) {
	if err := ctx.Err(); err != nil {
		return ProviderConfig{}, err
	}
	if token == nil || token.AccessToken == "" {
		return ProviderConfig{}, errors.New("selected authentication account has no access token")
	}
	// Bind and validate before evaluating any header expression. Errors from
	// schema/binding validation may contain private configuration values.
	provider, registration, configured, err := authenticationProviderTarget(before, owner)
	if err != nil {
		return ProviderConfig{}, err
	}
	if !configured {
		resolver, ok := before.runtime.resolver.(contextVariableResolver)
		if !ok {
			return ProviderConfig{}, errors.New("authentication header resolver does not support cancellation")
		}
		for _, name := range slices.Sorted(maps.Keys(provider.ExtraHeaders)) {
			value, err := resolver.ResolveValueContext(ctx, provider.ExtraHeaders[name])
			if err != nil {
				if ctx.Err() != nil {
					return ProviderConfig{}, ctx.Err()
				}
				return ProviderConfig{}, fmt.Errorf("resolving provider %s header %q failed", owner.ProviderID, name)
			}
			if value == "" {
				delete(provider.ExtraHeaders, name)
			} else {
				provider.ExtraHeaders[name] = value
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return ProviderConfig{}, err
	}
	applyOAuthTokenToProvider(&provider, cloneOAuthToken(token), registration)
	provider.APIKeyTemplate = ""
	return provider, nil
}

// authenticationRegistration supplies the exact configuration-bound refresher
// before account refresh. In particular, a target dropped for missing credentials
// still uses its captured original declarative Configuration. No headers run.
func authenticationRegistration(before AuthenticationCapture, owner providerregistry.RegistrationOwner) (providerregistry.Registration, error) {
	_, registration, _, err := authenticationProviderTarget(before, owner)
	return registration, err
}

func authenticationProviderTarget(before AuthenticationCapture, owner providerregistry.RegistrationOwner) (ProviderConfig, providerregistry.Registration, bool, error) {
	if err := validateAuthenticationCandidateOwner(before, owner); err != nil {
		return ProviderConfig{}, providerregistry.Registration{}, false, err
	}
	registration, ok := before.runtime.registry.Lookup(owner.ProviderID)
	if !ok || registration.Owner() != owner || registration.OAuth == nil {
		return ProviderConfig{}, providerregistry.Registration{}, false, errors.New("selected authentication owner does not support OAuth")
	}
	provider, configured := ProviderConfig{}, false
	provider, configured = before.runtime.config.authenticationCollectionProvider(owner.ProviderID)
	provider = cloneProviderConfig(provider)
	if configured {
		// Legacy accepted providers can have an implicit exact owner. Complete
		// only missing claims using the same captured registry as normal load.
		var err error
		provider, err = prepareConfiguredProviderOwner(owner.ProviderID, provider)
		if err != nil {
			return ProviderConfig{}, providerregistry.Registration{}, false, errors.New("authentication provider input has conflicting ownership")
		}
		provider = before.runtime.config.completeProviderOwner(owner.ProviderID, provider)
	} else {
		var err error
		provider, err = authenticationUnconfiguredProvider(before, owner, registration)
		if err != nil {
			return ProviderConfig{}, providerregistry.Registration{}, false, err
		}
	}
	bound, err := authenticationPreparedRegistration(before, owner, provider)
	return provider, bound, configured, err
}

func validateAuthenticationCandidateOwner(before AuthenticationCapture, owner providerregistry.RegistrationOwner) error {
	if before.runtime.IsClientOwned() {
		return ErrClientRuntimeManaged
	}
	if before.runtime.config == nil || owner.ProviderID == "" || !slices.Contains(before.owners, owner) {
		return errors.New("authentication owner is not in the accepted capture")
	}
	current, ok := before.runtime.ProviderOwner(owner.ProviderID)
	if !ok || current != owner {
		return errors.New("authentication provider owner changed")
	}
	return nil
}

// authenticationUnconfiguredProvider rebuilds the loader's known-provider
// overlay from its original target inputs and accepted catalog. It never tests
// the old catalog credential, resolves endpoints, or discovers/selects models.
func authenticationUnconfiguredProvider(before AuthenticationCapture, owner providerregistry.RegistrationOwner, registration providerregistry.Registration) (ProviderConfig, error) {
	cfg := before.runtime.config
	if cfg.providerScan == nil || cfg.authenticationBasis == nil || !cfg.authenticationBasis.valid || len(cfg.authenticationBasis.configured) == 0 {
		return ProviderConfig{}, errAuthenticationBasisUnavailable
	}
	var known catalog.Provider
	matches := 0
	for _, candidate := range cfg.providerScan.Providers {
		if string(candidate.ID) == owner.ProviderID {
			known = cloneProvider(candidate)
			matches++
		}
	}
	if matches != 1 {
		return ProviderConfig{}, errors.New("authentication provider has no unique accepted catalog")
	}
	var original struct {
		Providers map[string]ProviderConfig `json:"providers"`
	}
	decoder := json.NewDecoder(bytes.NewReader(cfg.authenticationBasis.configured))
	decoder.UseNumber()
	if err := decoder.Decode(&original); err != nil {
		return ProviderConfig{}, errAuthenticationBasisUnavailable
	}
	provider, exists := original.Providers[owner.ProviderID]
	provider = cloneProviderConfig(provider)
	if !exists && owner.HasPreset {
		provider.Owner = &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat}
		provider.Preset = &ProviderPresetReference{ID: owner.PresetID, Version: owner.PresetVersion, Digest: owner.PresetDigest}
	} else if !exists && registration.ProviderID != "" {
		provider.Owner = providerOwnerReferenceForRegistration(registration)
		if registration.Manifest != nil {
			provider.Plugin = &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}
		}
	}
	var err error
	provider, err = prepareConfiguredProviderOwner(owner.ProviderID, provider)
	if err != nil {
		return ProviderConfig{}, errors.New("authentication provider input has conflicting ownership")
	}
	provider = cfg.completeProviderOwner(owner.ProviderID, provider)
	if provider.BaseURL != "" {
		known.APIEndpoint = provider.BaseURL
	}
	if len(provider.Models) > 0 {
		models := make([]catalog.Model, 0, len(provider.Models)+len(known.Models))
		seen := make(map[string]bool)
		for _, model := range append(provider.Models, known.Models...) {
			if seen[model.ID] {
				continue
			}
			seen[model.ID] = true
			if model.Name == "" {
				model.Name = model.ID
			}
			models = append(models, model)
		}
		known.Models = models
	}
	headers := maps.Clone(known.DefaultHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}
	maps.Copy(headers, provider.ExtraHeaders)
	provider.ID, provider.Name, provider.Type = string(known.ID), known.Name, known.Type
	provider.BaseURL, provider.Models, provider.ExtraHeaders = known.APIEndpoint, known.Models, headers
	if provider.ExtraParams == nil {
		provider.ExtraParams = make(map[string]string)
	}
	return provider, nil
}

func authenticationPreparedRegistration(before AuthenticationCapture, owner providerregistry.RegistrationOwner, provider ProviderConfig) (providerregistry.Registration, error) {
	registration, ok := before.runtime.ProviderRegistrationFor(owner.ProviderID, provider)
	if !ok || registration.Owner() != owner || registration.OAuth == nil {
		return providerregistry.Registration{}, errors.New("authentication provider configuration or owner is invalid")
	}
	scratch := before.runtime.config.cloneForWrite()
	if scratch.Providers == nil {
		scratch.Providers = csync.NewMap[string, ProviderConfig]()
	}
	scratch.Providers.Set(owner.ProviderID, provider)
	if err := scratch.ValidateProviderConfiguration(owner.ProviderID, provider.Configuration); err != nil {
		return providerregistry.Registration{}, errors.New("authentication provider configuration is invalid")
	}
	return registration, nil
}

// authenticationConfigCandidate preserves the accepted generation except for
// the target's prepared authentication. A nil provider means logout. Callers
// must first validate scoped removal/fallbacks; finalize receipt and captured
// account authority before taking the runtime snapshot, then freeze this Config.
func authenticationConfigCandidate(before AuthenticationCapture, owner providerregistry.RegistrationOwner, prepared *ProviderConfig) (*Config, error) {
	if err := validateAuthenticationCandidateOwner(before, owner); err != nil {
		return nil, err
	}
	candidate := before.runtime.config.cloneForWrite()
	if prepared != nil {
		if prepared.OAuthToken == nil || prepared.OAuthToken.AccessToken == "" || prepared.APIKey != prepared.OAuthToken.AccessToken || prepared.APIKeyTemplate != "" {
			return nil, errors.New("prepared authentication provider has no literal selected OAuth credential")
		}
		if _, err := authenticationPreparedRegistration(before, owner, *prepared); err != nil {
			return nil, err
		}
		if candidate.Providers == nil {
			candidate.Providers = csync.NewMap[string, ProviderConfig]()
		}
		candidate.Providers.Set(owner.ProviderID, cloneProviderConfig(*prepared))
		return candidate, nil
	}
	if candidate.Providers == nil {
		return candidate, nil
	}
	provider, configured := candidate.Providers.Get(owner.ProviderID)
	if !configured {
		return candidate, nil
	}
	provider.APIKey, provider.APIKeyTemplate, provider.OAuthToken = "", "", nil
	provider.resolvedAPIKey = nil
	candidate.Providers.Set(owner.ProviderID, provider)
	return candidate.WithAuthenticationRevocation(owner)
}
