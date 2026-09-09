package config

import (
	"context"
	"errors"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/shell"
)

// authenticationCredentialFallback proves what the accepted catalog would
// supply after the staged scoped logout. It never invokes the loader, scans
// providers, resolves commands, or consults a process-global environment or
// catalog. The transaction must separately validate the accepted input basis,
// raw layered credential deletion, current publication, and file preimages.
func authenticationCredentialFallback(ctx context.Context, accepted RuntimeSnapshot, after *Config, owner providerregistry.RegistrationOwner) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if accepted.IsClientOwned() {
		return "", ErrClientRuntimeManaged
	}
	if accepted.config == nil || after == nil || owner.ProviderID == "" {
		return "", errors.New("authentication fallback requires an accepted configuration and exact owner")
	}
	current, ok := accepted.ProviderOwner(owner.ProviderID)
	if !ok || current != owner {
		return "", errors.New("authentication fallback owner changed")
	}
	if after.Providers != nil {
		provider, _ := after.Providers.Get(owner.ProviderID)
		if provider.APIKey != "" || provider.APIKeyTemplate != "" || provider.OAuthToken != nil {
			return "", errors.New("authentication logout still has layered credentials")
		}
	}
	if accepted.config.Options != nil && accepted.config.Options.DisableDefaultProviders {
		return "", nil
	}
	if accepted.config.providerScan == nil {
		return "", errors.New("authentication fallback has no accepted provider catalog; reload configuration")
	}
	var selected *catalog.Provider
	for i := range accepted.config.providerScan.Providers {
		provider := &accepted.config.providerScan.Providers[i]
		if string(provider.ID) != owner.ProviderID {
			continue
		}
		if selected != nil {
			return "", errors.New("authentication fallback catalog is ambiguous")
		}
		selected = provider
	}
	// A custom provider absent from the captured catalog does not inherit a
	// catalog key. An empty catalog key similarly needs no expansion policy.
	if selected == nil || selected.APIKey == "" {
		return "", nil
	}
	basis := accepted.config.authenticationBasis
	if basis == nil || !basis.valid || accepted.environment == nil {
		return "", errAuthenticationBasisUnavailable
	}
	// The ordinary resolver rejects a lone dollar despite shell.Document
	// treating it as literal. Preserve that production validation contract.
	if selected.APIKey == "$" {
		return "", errors.New("authentication catalog credential could not be resolved without commands")
	}
	fallback, err := shell.ExpandValueWithoutCommands(ctx, selected.APIKey, accepted.Environment(), basis.noUnset)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("authentication catalog credential could not be resolved without commands")
	}
	return fallback, nil
}

func validateAuthenticationLogoutFallback(ctx context.Context, accepted RuntimeSnapshot, after *Config, owner providerregistry.RegistrationOwner) error {
	fallback, err := authenticationCredentialFallback(ctx, accepted, after, owner)
	if err != nil {
		return err
	}
	if fallback != "" {
		return errors.New("authentication logout would restore a catalog or environment credential; remove that credential source before logging out")
	}
	return nil
}
