package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
)

// The fixed receipt removes only this scope's OAuth value. Any inherited OAuth
// or key shadow is rejected by the ordinary merged postimage before writing.
func checkedAPIKeyFields(provider ProviderConfig, slots ...ProviderCredentialSlot) (map[string]any, error) {
	slot := ProviderCredentialSlot{ID: "provider.api_key"}
	if len(slots) > 0 {
		slot = slots[0]
	}
	if !checkedProviderCredentialMatches(provider, slot) || provider.Owner == nil {
		return nil, errResolvedProviderAPIKeyStale
	}
	fields := map[string]any{"owner": provider.Owner}
	if slot.Property == "" {
		fields["api_key"], fields["oauth"] = provider.resolvedAPIKey.source, nil
	} else {
		fields["configuration"] = map[string]any{slot.Property: provider.resolvedCredentials[slot.ID].source}
	}
	if provider.Plugin != nil {
		fields["plugin"] = provider.Plugin
	}
	if provider.Preset != nil {
		fields["preset"] = provider.Preset
	}
	return fields, nil
}

func applyCheckedAPIKeyFields(data []byte, providerID string, fields map[string]any) ([]byte, error) {
	if len(data) == 0 {
		data = []byte("{}")
	}
	for _, field := range slices.Sorted(maps.Keys(fields)) {
		if field == "configuration" {
			for property, source := range fields[field].(map[string]any) {
				encoded, err := json.Marshal(source)
				if err != nil {
					return nil, errors.New("checked credential source cannot be encoded")
				}
				data, err = runtimeControlChangeField(data, []string{"providers", providerID, "configuration", property}, encoded, false)
				if err != nil {
					return nil, errors.New("checked credential scope must contain objects")
				}
			}
			continue
		}
		value, err := json.Marshal(fields[field])
		if err != nil {
			return nil, errors.New("checked provider fields cannot be encoded")
		}
		data, err = runtimeControlChangeField(data, []string{"providers", providerID, field}, value, field == "oauth")
		if err != nil {
			return nil, errors.New("checked provider scope must contain objects")
		}
	}
	return data, nil
}

func (layers authenticationLayers) stageCheckedAPIKey(ctx context.Context, path string, provider ProviderConfig, writtenPaths []string, slots ...ProviderCredentialSlot) (authenticationCredentialEdit, error) {
	if err := ctx.Err(); err != nil {
		return authenticationCredentialEdit{}, err
	}
	if !filepath.IsAbs(path) || isShellConfig(path) || !slices.Contains(layers.order, path) || !slices.Contains(writtenPaths, path) {
		return authenticationCredentialEdit{}, errors.New("checked provider requires its captured writable JSON scope")
	}
	slot := ProviderCredentialSlot{ID: "provider.api_key"}
	if len(slots) > 0 {
		slot = slots[0]
	}
	fields, err := checkedAPIKeyFields(provider, slot)
	if err != nil {
		return authenticationCredentialEdit{}, err
	}
	before, err := layers.merged("", nil)
	if err != nil {
		return authenticationCredentialEdit{}, err
	}
	data, err := applyCheckedAPIKeyFields(layers.values[path], provider.ID, fields)
	if err != nil {
		return authenticationCredentialEdit{}, err
	}
	replacements := make(map[string][]byte, len(writtenPaths))
	for _, written := range writtenPaths {
		replacements[written] = data
	}
	after, err := layers.mergedReplacements(replacements)
	if err != nil {
		return authenticationCredentialEdit{}, err
	}
	actual, present := after.Providers.Get(provider.ID)
	if !present || !providerOwnershipReferencesMatch(actual, provider) {
		return authenticationCredentialEdit{}, errors.New("checked provider credentials or ownership are shadowed by another configuration layer")
	}
	if slot.Property == "" {
		if actual.APIKey != provider.resolvedAPIKey.source || actual.OAuthToken != nil {
			return authenticationCredentialEdit{}, errors.New("checked provider credential is shadowed by another configuration layer")
		}
	} else {
		original, _ := before.Providers.Get(provider.ID)
		value, isString := actual.Configuration[slot.Property].(string)
		if !isString || value != provider.resolvedCredentials[slot.ID].source || actual.APIKey != original.APIKey || !reflect.DeepEqual(actual.OAuthToken, original.OAuthToken) {
			return authenticationCredentialEdit{}, errors.New("checked configuration credential is shadowed or changes another credential")
		}
	}
	return authenticationCredentialEdit{path: path, data: bytes.Clone(data), before: before, after: after}, ctx.Err()
}

func (c *Config) advanceAuthenticationBasisCheckedAPIKey(path string, provider ProviderConfig, slots ...ProviderCredentialSlot) {
	basis := c.authenticationBasis.clone()
	if basis == nil {
		return
	}
	c.authenticationBasis = basis
	path = filepath.Clean(path)
	source, found := basis.sources[path]
	fields, err := checkedAPIKeyFields(provider, slots...)
	if !found || isShellConfig(path) || err != nil {
		basis.valid = false
		return
	}
	data, err := applyCheckedAPIKeyFields(source.raw, provider.ID, fields)
	if err != nil {
		basis.valid = false
		return
	}
	basis.sources[path] = authenticationBasisSource{exists: true, raw: data, evaluated: bytes.Clone(data)}
}

func validateCheckedCredentialTopology(original authenticationLayers, edit authenticationCredentialEdit, provider ProviderConfig, paths []string, slot ProviderCredentialSlot) error {
	if slot.Property == "" {
		return validateAuthenticationTopologyEffect(original, edit, provider.ID, &provider, paths)
	}
	baseline, err := original.merged("", nil)
	if err != nil {
		return err
	}
	left, err := json.Marshal(baseline)
	if err != nil {
		return errors.New("checked credential topology cannot be compared")
	}
	right, err := json.Marshal(edit.after)
	if err != nil {
		return errors.New("checked credential topology cannot be compared")
	}
	fields := [][]string{{"configuration", slot.Property}, {"owner"}, {"plugin"}, {"preset"}}
	for _, field := range fields {
		path := append([]string{"providers", provider.ID}, field...)
		left, err = runtimeControlChangeField(left, path, nil, true)
		if err != nil {
			return errors.New("checked credential topology cannot be compared")
		}
		right, err = runtimeControlChangeField(right, path, nil, true)
		if err != nil {
			return errors.New("checked credential topology cannot be compared")
		}
	}
	left, err = normalizeCredentialComparisonContainers(left, provider.ID)
	if err != nil {
		return err
	}
	right, err = normalizeCredentialComparisonContainers(right, provider.ID)
	if err != nil {
		return err
	}
	if !RuntimeControlJSONEqual(left, right) {
		return errors.New("checked credential scope changes unrelated effective configuration")
	}
	return nil
}

// A first property save may create its containing objects. After removing the
// selected field, absent and empty containers on only that path are equivalent.
func normalizeCredentialComparisonContainers(data []byte, providerID string) ([]byte, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, errors.New("checked credential topology cannot be compared")
	}
	providers, _ := root["providers"].(map[string]any)
	provider, _ := providers[providerID].(map[string]any)
	if configuration, ok := provider["configuration"].(map[string]any); ok && len(configuration) == 0 {
		delete(provider, "configuration")
	}
	if len(provider) == 0 {
		delete(providers, providerID)
	}
	if len(providers) == 0 {
		delete(root, "providers")
	}
	return json.Marshal(root)
}
