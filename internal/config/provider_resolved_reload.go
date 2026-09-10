package config

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Only generic field updates opt into retaining unchanged resolved inputs.
// Explicit reload remains an instruction to resolve authored sources afresh.
func resolvedInputRetentionForFields(previous *Config, fields map[string]any) (*Config, error) {
	if previous == nil {
		return nil, nil
	}
	retained := previous.cloneForWrite()
	for id, provider := range resolvedInputProviders(retained) {
		if provider.resolvedAPIKey == nil && provider.resolvedEndpoint == nil && len(provider.resolvedCredentials) == 0 {
			continue
		}
		unchanged, err := resolvedInputFieldsUntouched(id, fields)
		if err != nil {
			return nil, err
		}
		if !unchanged["api_key"] || !unchanged["oauth"] || !unchanged["owner"] || !unchanged["plugin"] || !unchanged["preset"] || !unchanged["id"] {
			provider.resolvedAPIKey = nil
		}
		if !unchanged["base_url"] || !unchanged["owner"] || !unchanged["plugin"] || !unchanged["preset"] || !unchanged["id"] {
			provider.resolvedEndpoint = nil
		}
		if !unchanged["owner"] || !unchanged["plugin"] || !unchanged["preset"] || !unchanged["id"] {
			provider.resolvedCredentials = nil
		} else {
			untouched, err := resolvedConfigurationFieldsUntouched(id, provider, fields)
			if err != nil {
				return nil, err
			}
			for slot := range provider.resolvedCredentials {
				if !untouched[slot] {
					delete(provider.resolvedCredentials, slot)
				}
			}
		}
		public := false
		if retained.Providers != nil {
			_, public = retained.Providers.Get(id)
		}
		if public {
			retained.Providers.Set(id, provider)
		} else {
			retained.authenticationCandidates[id] = provider
		}
	}
	return retained, nil
}

// Logged-out OAuth candidates retain the same captured credential and endpoint
// proofs as configured providers, without becoming public/available entries.
func resolvedInputProviders(cfg *Config) map[string]ProviderConfig {
	if cfg == nil {
		return nil
	}
	providers := make(map[string]ProviderConfig, len(cfg.authenticationCandidates))
	for id, provider := range cfg.authenticationCandidates {
		providers[id] = provider
	}
	if cfg.Providers != nil {
		for id, provider := range cfg.Providers.Seq2() {
			providers[id] = provider
		}
	}
	return providers
}

// Apply the existing generic setter's exact JSONPath semantics to markers.
// This recognizes parent replacement and escaped provider IDs, including an
// explicit same-value credential write, without inventing a second path parser.
func resolvedInputFieldsUntouched(id string, fields map[string]any) (map[string]bool, error) {
	names := []string{"api_key", "oauth", "base_url", "owner", "plugin", "preset", "id"}
	unchanged := map[string]bool{}
	for _, name := range names {
		unchanged[name] = true
	}
	// Two different markers prevent an explicit caller value from accidentally
	// matching the marker and looking like an untouched field.
	for _, sentinel := range []string{"private-resolved-input-one", "private-resolved-input-two"} {
		marker := map[string]string{}
		for _, name := range names {
			marker[name] = sentinel
		}
		data, err := json.Marshal(map[string]any{"providers": map[string]any{id: marker}})
		if err != nil {
			return nil, errors.New("resolved provider input update cannot be checked")
		}
		for _, key := range slices.Sorted(maps.Keys(fields)) {
			data, err = sjson.SetBytes(data, key, fields[key])
			if err != nil {
				return nil, errors.New("resolved provider input update cannot be checked")
			}
		}
		provider := gjson.ParseBytes(data).Get("providers").Map()[id]
		for _, name := range names {
			value := provider.Get(name)
			unchanged[name] = unchanged[name] && value.Type == gjson.String && value.String() == sentinel
		}
	}
	return unchanged, nil
}

// Restore only independently proven source/owner matches before loader
// credential/endpoint evaluation. Untouched sources or owners that changed
// outside this setter fail visibly; only explicit replacement or reload may
// authorize new source evaluation.
func (next *Config) retainResolvedProviderInputs(previous *Config) error {
	if previous == nil {
		return nil
	}
	snapshot := RuntimeSnapshot{config: next, registry: next.providerCapabilities()}
	for id, old := range resolvedInputProviders(previous) {
		if old.resolvedAPIKey == nil && old.resolvedEndpoint == nil && len(old.resolvedCredentials) == 0 {
			continue
		}
		provider, exists := ProviderConfig{}, false
		if next.Providers != nil {
			provider, exists = next.Providers.Get(id)
		}
		if !exists {
			return errResolvedProviderAPIKeyStale
		}
		var err error
		provider, err = prepareConfiguredProviderOwner(id, provider)
		if err != nil {
			return errors.New("resolved provider input owner is invalid")
		}
		provider = next.completeProviderOwner(id, provider)
		provider.ID = id
		owner, active := snapshot.ProviderOwnerFor(id, provider)
		if !active {
			return errResolvedProviderAPIKeyStale
		}
		if old.resolvedAPIKey != nil {
			if !old.resolvedAPIKey.matches(old) || old.resolvedAPIKey.owner != owner || !providerOwnershipReferencesMatch(old, provider) || provider.APIKey != old.resolvedAPIKey.source || provider.OAuthToken != nil {
				return errResolvedProviderAPIKeyStale
			}
			provider.APIKey, provider.APIKeyTemplate, provider.resolvedAPIKey = old.APIKey, old.APIKeyTemplate, old.resolvedAPIKey
		}
		for id, binding := range old.resolvedCredentials {
			if binding == nil {
				return errResolvedConfigurationCredential
			}
			source, present := provider.Configuration[binding.property].(string)
			if !binding.matches(old) || binding.owner != owner || !providerOwnershipReferencesMatch(old, provider) || !present || source != binding.source {
				return errResolvedConfigurationCredential
			}
			if provider.resolvedCredentials == nil {
				provider.resolvedCredentials = make(map[string]*resolvedProviderConfigurationCredential)
			}
			provider.Configuration[binding.property] = binding.literal
			provider.resolvedCredentials[id] = binding
		}
		endpointSource := provider.BaseURL
		if endpointSource == "" && next.providerScan != nil && (next.Options == nil || !next.Options.DisableDefaultProviders) {
			for _, catalogue := range next.providerScan.Providers {
				if string(catalogue.ID) == id {
					endpointSource = catalogue.APIEndpoint
					break
				}
			}
		}
		if old.resolvedEndpoint != nil {
			if !old.resolvedEndpoint.matches(old) || old.resolvedEndpoint.owner != owner || !providerOwnershipReferencesMatch(old, provider) || endpointSource != old.resolvedEndpoint.source {
				return errResolvedProviderEndpointStale
			}
			provider.BaseURL, provider.resolvedEndpoint = old.BaseURL, old.resolvedEndpoint
		}
		next.Providers.Set(id, provider)
	}
	return nil
}

func resolvedConfigurationFieldsUntouched(id string, provider ProviderConfig, fields map[string]any) (map[string]bool, error) {
	untouched := map[string]bool{}
	for slot := range provider.resolvedCredentials {
		untouched[slot] = true
	}
	for _, sentinel := range []string{"private-config-credential-one", "private-config-credential-two"} {
		properties := map[string]string{}
		for _, binding := range provider.resolvedCredentials {
			if binding == nil {
				return nil, errResolvedConfigurationCredential
			}
			properties[binding.property] = sentinel
		}
		data, err := json.Marshal(map[string]any{"providers": map[string]any{id: map[string]any{"configuration": properties}}})
		if err != nil {
			return nil, errResolvedConfigurationCredential
		}
		for _, key := range slices.Sorted(maps.Keys(fields)) {
			data, err = sjson.SetBytes(data, key, fields[key])
			if err != nil {
				return nil, errResolvedConfigurationCredential
			}
		}
		actual := gjson.ParseBytes(data).Get("providers").Map()[id].Get("configuration").Map()
		for slot, binding := range provider.resolvedCredentials {
			value := actual[binding.property]
			untouched[slot] = untouched[slot] && value.Type == gjson.String && value.String() == sentinel
		}
	}
	return untouched, nil
}
