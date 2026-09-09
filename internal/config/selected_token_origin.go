package config

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/tidwall/gjson"
)

// RefreshProviderOAuthTokenForRuntime honors the caller's explicit persistence
// scope. It never substitutes an automatic credential origin for that choice.
func (s *ConfigStore) RefreshProviderOAuthTokenForRuntime(ctx context.Context, scope Scope, owner providerregistry.RegistrationOwner, expected *oauth.Token, admitted RuntimeSnapshot) (*oauth.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.configPath(scope)
	if err != nil {
		return nil, err
	}
	return s.refreshProviderOAuthTokenAtPath(ctx, path, scope, nil, owner, expected, admitted)
}

// RefreshProviderOAuthTokenAtOrigin is for automatic owning-client refresh.
// The accepted load's merge order and source bytes select the exact credential
// file, including discovered project layers outside the two manual scopes.
// Conflicting or generated origins fail before an exchange; no default path is
// invented. The transaction rechecks this origin against its current runtime.
func (s *ConfigStore) RefreshProviderOAuthTokenAtOrigin(ctx context.Context, owner providerregistry.RegistrationOwner, expected *oauth.Token, admitted RuntimeSnapshot) (*oauth.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := selectedTokenCredentialOrigin(admitted, owner)
	if err != nil {
		return nil, err
	}
	// Preserve existing receipts for the two explicit scopes. Other captured
	// source paths have their own finite identity in the path-scoped journal.
	var identity any = struct{ Origin string }{path}
	for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace} {
		if scoped, err := s.configPath(scope); err == nil && scoped == path {
			identity = scope
			break
		}
	}
	validate := func(current RuntimeSnapshot) error {
		actual, err := selectedTokenCredentialOrigin(current, owner)
		if err != nil || actual != path {
			return errors.New("selected OAuth credential origin changed; recollect the owning client runtime")
		}
		return nil
	}
	return s.refreshProviderOAuthTokenAtPath(ctx, path, identity, validate, owner, expected, admitted)
}

// This is a pure read of immutable load evidence. It never discovers paths,
// reads today's files, resolves expressions, or touches the account database.
func selectedTokenCredentialOrigin(snapshot RuntimeSnapshot, owner providerregistry.RegistrationOwner) (string, error) {
	if snapshot.IsClientOwned() {
		return "", ErrClientRuntimeManaged
	}
	cfg := snapshot.Config()
	if cfg == nil || owner.ProviderID == "" || owner.AccountNamespace != "" || !owner.HasOAuth {
		return "", errors.New("OAuth credential origin requires a namespace-free owner")
	}
	actual, found := snapshot.ProviderOwner(owner.ProviderID)
	provider, configured := cfg.Providers.Get(owner.ProviderID)
	if !found || actual != owner || !configured || provider.Disable || provider.OAuthToken == nil || provider.resolvedAPIKey != nil || provider.APIKey != provider.OAuthToken.AccessToken {
		return "", errors.New("selected OAuth credential has no coherent captured origin")
	}
	if err := validateRemoteOAuthToken(provider.OAuthToken); err != nil {
		return "", err
	}
	basis := cfg.authenticationBasis
	if basis == nil || !basis.valid {
		return "", errAuthenticationBasisUnavailable
	}
	var tokenPath, keyPath string
	for _, path := range basis.order {
		source, known := basis.sources[path]
		if !known {
			return "", errAuthenticationBasisUnavailable
		}
		if !source.exists || len(source.evaluated) == 0 {
			continue
		}
		root := gjson.ParseBytes(source.evaluated)
		if !authenticationLayerObject(source.evaluated) || selectedTokenAliasedField(root, "providers") {
			return "", errors.New("OAuth credential origin has ambiguous source fields")
		}
		providers := root.Get("providers")
		if providers.Exists() && !providers.IsObject() {
			return "", errors.New("OAuth credential origin has a non-object provider layer")
		}
		value := providers.Get(owner.ProviderID)
		if !value.Exists() {
			continue
		}
		if !value.IsObject() || selectedTokenAliasedField(value, "oauth", "api_key") {
			return "", errors.New("OAuth credential origin has ambiguous credential fields")
		}
		if value.Get("oauth").Exists() {
			tokenPath = path
		}
		if value.Get("api_key").Exists() {
			keyPath = path
		}
	}
	if tokenPath == "" || tokenPath != keyPath {
		return "", errors.New("OAuth credential fields do not have one captured origin; reconcile the selected configuration layers")
	}
	if !filepath.IsAbs(tokenPath) || filepath.Clean(tokenPath) != tokenPath || isShellConfig(tokenPath) {
		return "", errors.New("OAuth credential origin is not a writable captured JSON source")
	}
	source := basis.sources[tokenPath]
	file := authenticationInputFile{path: tokenPath, data: source.raw, info: authenticationInputFileInfo{exists: source.exists}}
	if err := selectedTokenDiskCredential(file, owner.ProviderID, provider, provider.OAuthToken); err != nil {
		return "", errors.New("OAuth credential origin does not contain the complete selected token")
	}
	return tokenPath, nil
}
