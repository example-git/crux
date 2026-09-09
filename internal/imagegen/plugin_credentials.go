package imagegen

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"reflect"
	"strings"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
)

type PluginCredentialBindings struct {
	Providers map[string]providerregistry.RegistrationOwner
	Browser   func(context.Context, manifest.ImageCredential) (http.CookieJar, string, error)
}

func ResolvePluginCredentials(ctx context.Context, store *config.ConfigStore, bundle providerplugin.RegisteredImageBundle, bindings PluginCredentialBindings) (PluginCredentials, error) {
	if store == nil {
		return PluginCredentials{}, errors.New("image credential configuration is unavailable")
	}
	snapshot := store.RuntimeSnapshot()
	return resolvePluginCredentialsSnapshot(ctx, store, snapshot, bundle, bindings)
}

func resolvePluginCredentialsSnapshot(ctx context.Context, store *config.ConfigStore, snapshot config.RuntimeSnapshot, bundle providerplugin.RegisteredImageBundle, bindings PluginCredentialBindings) (PluginCredentials, error) {
	environment := map[string]string{}
	for _, entry := range snapshot.Environment() {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			environment[name] = value
		}
	}
	result := PluginCredentials{Values: map[string]any{}, CookieJars: map[string]http.CookieJar{}}
	refreshState := newImageCredentialRefresh(store)
	identities := map[string]any{}
	validators := []func() error{}
	for _, declaration := range bundle.Manifest.Credentials {
		if err := ctx.Err(); err != nil {
			return PluginCredentials{}, err
		}
		switch declaration.Source {
		case "environment":
			value := environment[declaration.Environment]
			if value == "" {
				return PluginCredentials{}, errors.New("declared image environment credential is unavailable")
			}
			result.Values[declaration.ID] = value
		case "provider":
			credentialSnapshot := snapshot
			refreshSpent := false
			owner, ok := bindings.Providers[declaration.ID]
			if !ok || owner.ProviderID != declaration.Provider {
				return PluginCredentials{}, errors.New("image provider credential requires an explicit exact owner binding")
			}
			if err := validateImageCredentialOwner(store, credentialSnapshot, owner); err != nil {
				return PluginCredentials{}, err
			}
			provider, ok := credentialSnapshot.Config().Providers.Get(owner.ProviderID)
			if !ok {
				return PluginCredentials{}, errors.New("bound image credential provider is unavailable")
			}
			if provider.OAuthToken != nil && provider.OAuthToken.IsExpired() {
				if credentialSnapshot.IsClientOwned() {
					refreshed, err := store.RequestClientRefresh(ctx, credentialSnapshot, owner)
					if err != nil {
						return PluginCredentials{}, err
					}
					credentialSnapshot = refreshed
					refreshSpent = true
				} else {
					if _, err := store.RefreshOAuthTokenForOwner(ctx, config.ScopeGlobal, owner); err != nil {
						return PluginCredentials{}, err
					}
					credentialSnapshot = store.RuntimeSnapshot()
				}
			}
			values, err := imageProviderCredential(ctx, credentialSnapshot, owner)
			if err != nil {
				return PluginCredentials{}, err
			}
			result.Values[declaration.ID] = values
			if credentialSnapshot.IsClientOwned() && provider.OAuthToken != nil {
				refreshState.bind(declaration.ID, owner, credentialSnapshot, refreshSpent)
			}
			identities[declaration.ID] = owner
			validators = append(validators, func() error {
				if credentialSnapshot.IsClientOwned() {
					return ctx.Err()
				}
				if err := store.ValidateActiveProviderOwner(owner); err != nil {
					return err
				}
				current, err := imageProviderCredential(ctx, store.RuntimeSnapshot(), owner)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(current, values) {
					return errors.New("image provider credential changed during execution")
				}
				return nil
			})
		case "browser":
			if bindings.Browser == nil {
				return PluginCredentials{}, errors.New("image browser credential requires a process-local selection")
			}
			jar, identity, err := bindings.Browser(ctx, declaration)
			if err != nil {
				return PluginCredentials{}, err
			}
			if jar == nil || identity == "" {
				return PluginCredentials{}, errors.New("image browser credential selection is incomplete")
			}
			result.CookieJars[declaration.ID] = jar
			identities[declaration.ID] = identity
		default:
			return PluginCredentials{}, errors.New("unsupported image credential source")
		}
	}
	redact.RegisterJSONValue(result.Values)
	if len(refreshState.owners) > 0 {
		refreshState.values = maps.Clone(result.Values)
		result.ReadValues = refreshState.read
		result.Refresh = refreshState.refresh
	}
	digest, err := imageSessionIdentity([32]byte{}, PluginCredentials{Values: identities})
	if err != nil {
		return PluginCredentials{}, err
	}
	result.Identity = hex.EncodeToString(digest[:])
	result.Validate = func() error {
		for _, validate := range validators {
			if err := validate(); err != nil {
				return err
			}
		}
		return nil
	}
	if err := result.Validate(); err != nil {
		return PluginCredentials{}, err
	}
	return result, nil
}

func imageProviderCredential(ctx context.Context, snapshot config.RuntimeSnapshot, owner providerregistry.RegistrationOwner) (map[string]any, error) {
	cfg := snapshot.Config()
	if cfg == nil || cfg.Providers == nil {
		return nil, errors.New("image credential provider configuration is unavailable")
	}
	provider, ok := cfg.Providers.Get(owner.ProviderID)
	actual, active := snapshot.ProviderOwnerFor(owner.ProviderID, provider)
	if !ok || !active || provider.Disable || actual != owner {
		return nil, errors.New("image credential provider owner is unavailable")
	}
	key, err := snapshot.ResolveProviderAPIKey(provider)
	if err != nil {
		return nil, errors.New("image provider API credential resolution failed")
	}
	access := ""
	if provider.OAuthToken != nil {
		access = provider.OAuthToken.AccessToken
	}
	if key == "" && access == "" {
		return nil, errors.New("image provider has no usable credential")
	}
	baseURL, err := snapshot.ResolveProviderEndpoint(provider)
	if err != nil {
		return nil, errors.New("image provider endpoint resolution failed")
	}
	result := map[string]any{"api_key": key, "access_token": access, "base_url": strings.TrimRight(baseURL, "/"), "account": map[string]any{}}
	explicitAPIKey, err := snapshot.UsesResolvedProviderAPIKey(owner.ProviderID)
	if err != nil {
		return nil, errors.New("image provider API credential changed")
	}
	if owner.AccountNamespace != "" && !explicitAPIKey {
		entry, forwarded := snapshot.EphemeralAccount(owner)
		if !forwarded && !snapshot.IsClientOwned() {
			entry, err = accounts.Active(ctx, owner.AccountNamespace)
			if err != nil {
				return nil, errors.New("image provider account lookup failed")
			}
		}
		if snapshot.IsClientOwned() && access != "" && (entry == nil || entry.AccessToken != access) {
			return nil, errors.New("captured image OAuth account metadata is missing or mismatched")
		}
		if entry != nil && (entry.AccessToken == access || entry.AccessToken == key) && len(entry.Raw) > 0 {
			if len(entry.Raw) > 1<<20 {
				return nil, errors.New("image provider account metadata exceeds limit")
			}
			decoder := json.NewDecoder(bytes.NewReader(entry.Raw))
			decoder.UseNumber()
			var raw map[string]any
			if decoder.Decode(&raw) != nil || decoder.Decode(new(any)) != io.EOF {
				return nil, errors.New("invalid image provider account metadata")
			}
			result["account"] = raw
		}
	}
	return result, nil
}
