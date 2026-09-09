package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
)

// The fixed receipt removes only this scope's OAuth value. Any inherited OAuth
// or key shadow is rejected by the ordinary merged postimage before writing.
func checkedAPIKeyFields(provider ProviderConfig) (map[string]any, error) {
	if !provider.resolvedAPIKey.matches(provider) || provider.Owner == nil {
		return nil, errResolvedProviderAPIKeyStale
	}
	fields := map[string]any{"api_key": provider.resolvedAPIKey.source, "oauth": nil, "owner": provider.Owner}
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
func (layers authenticationLayers) stageCheckedAPIKey(ctx context.Context, path string, provider ProviderConfig, writtenPaths []string) (authenticationCredentialEdit, error) {
	if err := ctx.Err(); err != nil {
		return authenticationCredentialEdit{}, err
	}
	if !filepath.IsAbs(path) || isShellConfig(path) || !slices.Contains(layers.order, path) || !slices.Contains(writtenPaths, path) {
		return authenticationCredentialEdit{}, errors.New("checked provider requires its captured writable JSON scope")
	}
	fields, err := checkedAPIKeyFields(provider)
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
	if !present || actual.APIKey != provider.resolvedAPIKey.source || actual.OAuthToken != nil || !providerOwnershipReferencesMatch(actual, provider) {
		return authenticationCredentialEdit{}, errors.New("checked provider credentials or ownership are shadowed by another configuration layer")
	}
	return authenticationCredentialEdit{path: path, data: bytes.Clone(data), before: before, after: after}, ctx.Err()
}
func (c *Config) advanceAuthenticationBasisCheckedAPIKey(path string, provider ProviderConfig) {
	basis := c.authenticationBasis.clone()
	if basis == nil {
		return
	}
	c.authenticationBasis = basis
	path = filepath.Clean(path)
	source, found := basis.sources[path]
	fields, err := checkedAPIKeyFields(provider)
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
