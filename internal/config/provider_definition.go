package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
)

// ClientProviderDefinition exports the exact captured provider definition
// without reading accounts or reopening bundle files. Refresh preconditions and
// whole-runtime collection must use the same representation.
func (snapshot RuntimeSnapshot) ClientProviderDefinition(id string) (RemoteProviderDefinition, providerregistry.RegistrationOwner, error) {
	return snapshot.clientProviderDefinition(context.Background(), id, snapshot.Resolve)
}

func (snapshot RuntimeSnapshot) clientProviderDefinition(ctx context.Context, id string, resolve func(string) (string, error)) (RemoteProviderDefinition, providerregistry.RegistrationOwner, error) {
	definition, owner, err := snapshot.clientProviderDefinitionRaw(id)
	if err != nil {
		return definition, owner, err
	}
	if nativeConstruction(owner.Construction) {
		identity, identityErr := snapshot.nativeIdentities.resolve(ctx, owner.Construction)
		if identityErr != nil {
			return RemoteProviderDefinition{}, providerregistry.RegistrationOwner{}, identityErr
		}
		definition.NativeIdentity = &identity
	}
	provider, _ := snapshot.config.authenticationCollectionProvider(id)
	if err := snapshot.validateResolvedProviderEndpointOwner(provider); err != nil {
		return RemoteProviderDefinition{}, providerregistry.RegistrationOwner{}, err
	}
	definition.Config.BaseURL, err = ResolveProviderEndpoint(provider, resolve)
	if err != nil {
		return RemoteProviderDefinition{}, providerregistry.RegistrationOwner{}, errors.New("selected client endpoint cannot be resolved")
	}
	if definition.Config.BaseURL == "" && definition.Config.Owner.Type == ProviderOwnerCore {
		switch owner.Construction {
		case providerregistry.ConstructionCodex:
			definition.Config.BaseURL = codex.APIEndpoint
		case providerregistry.ConstructionGeminiAntigravity:
			definition.Config.BaseURL = gemini.APIEndpoint
		}
	}
	return definition, owner, nil
}

// Raw definitions are compared while account/config locks are held. They use
// immutable config/scan/environment data only and never acquire ConfigStore.writeMu.
func (snapshot RuntimeSnapshot) clientProviderDefinitionRaw(id string) (RemoteProviderDefinition, providerregistry.RegistrationOwner, error) {
	var zero RemoteProviderDefinition
	var noOwner providerregistry.RegistrationOwner
	if snapshot.IsClientOwned() {
		return zero, noOwner, errors.New("collect provider definition on its owning client")
	}
	cfg := snapshot.Config()
	if cfg == nil {
		return zero, noOwner, errors.New("captured provider runtime is required")
	}
	provider, ok := cfg.authenticationCollectionProvider(id)
	if !ok {
		return zero, noOwner, fmt.Errorf("selected client provider %q is unavailable", id)
	}
	owner, ok := snapshot.ProviderOwnerFor(id, provider)
	if !ok {
		return zero, noOwner, errors.New("selected client provider has no active exact owner")
	}
	if err := snapshot.validateResolvedConfigurationCredentials(provider); err != nil {
		return zero, noOwner, err
	}
	definition := RemoteProviderDefinition{Config: cloneProviderConfig(provider)}
	if owner.Construction == providerregistry.ConstructionGeminiAntigravity {
		project := snapshot.Getenv("GEMINI_PROJECT_ID")
		definition.GeminiProjectID = &project
	}
	if provider.Plugin != nil {
		if cfg.providerScan == nil {
			return zero, noOwner, errors.New("selected provider scan is unavailable")
		}
		status, ok := cfg.providerScan.pluginStatuses[provider.Plugin.ID]
		if !ok || status.State != providerplugin.StateRegistered || status.Version != provider.Plugin.Version {
			return zero, noOwner, errors.New("selected provider bundle generation is unavailable")
		}
		definition.BundleDigest = status.Digest
	} else if provider.Preset != nil {
		definition.BundleDigest = provider.Preset.Digest
	}
	definition.Config.APIKey, definition.Config.APIKeyTemplate, definition.Config.OAuthToken = "", "", nil
	definition.Config.resolvedAPIKey = nil
	definition.Config.resolvedEndpoint = nil
	definition.Config.resolvedCredentials = nil
	return definition, owner, nil
}

// An explicit, loaded OAuth definition remains collectable before login. The
// candidate is not inserted into Config.Providers, so local readiness,
// migration and authentication status retain their unconfigured behavior.
func (c *Config) authenticationCollectionProvider(id string) (ProviderConfig, bool) {
	if c == nil {
		return ProviderConfig{}, false
	}
	if c.Providers != nil {
		if provider, ok := c.Providers.Get(id); ok {
			return provider, true
		}
	}
	provider, ok := c.authenticationCandidates[id]
	return cloneProviderConfig(provider), ok
}

// ProviderDefinitionDigest binds executable provider configuration and the
// exact bundle digest without including the separately carried account token.
func (p RemoteRuntimeProposal) ProviderDefinitionDigest(providerID string) (string, error) {
	for _, definition := range p.Providers {
		if definition.Config.ID == providerID {
			return definition.Digest()
		}
	}
	return "", errors.New("provider definition is absent from the client runtime")
}

func (definition RemoteProviderDefinition) Digest() (string, error) {
	data, err := json.Marshal(definition)
	if err != nil {
		return "", errors.New("provider definition cannot be encoded")
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
