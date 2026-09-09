package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
)

// ErrAuthenticationProviderDisabled is an explicit maintenance denial, rather
// than a logout or an instruction to choose another model or enable a provider.
var ErrAuthenticationProviderDisabled = errors.New("provider remains disabled after authentication maintenance")

// This immutable authority belongs to a Config generation. Keeping it on only
// one RuntimeSnapshot would lose it when UpdateModels reconstructs the runtime.
type authenticationRuntimeAccounts struct {
	target  providerregistry.RegistrationOwner
	entries map[providerregistry.RegistrationOwner]*accounts.Entry
}

func (authenticationRuntimeAccounts) MarshalJSON() ([]byte, error) {
	return nil, errors.New("runtime authentication accounts are private")
}
func (authenticationRuntimeAccounts) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private runtime authentication accounts]"))
}

func cloneConstructionAccount(entry *accounts.Entry) *accounts.Entry {
	if entry == nil {
		return nil
	}
	copy := *entry
	copy.Raw = slices.Clone(entry.Raw)
	return &copy
}

// finalizeRuntimeAuthenticationAccounts changes only an unpublished candidate.
// The caller must finish all candidate/basis work before exposing its snapshot.
func (c AuthenticationCapture) finalizeRuntimeAuthenticationAccounts(candidate *Config, target providerregistry.RegistrationOwner, desired *accounts.Entry) error {
	if c.runtime.IsClientOwned() {
		return ErrClientRuntimeManaged
	}
	if candidate == nil || c.runtime.config == nil || candidate == c.runtime.config {
		return errors.New("runtime authentication requires an unpublished configuration candidate")
	}
	if target.ProviderID == "" || !slices.Contains(c.owners, target) {
		return errors.New("runtime authentication target was not captured")
	}
	if current, ok := candidate.ProviderOwner(target.ProviderID); !ok || current != target {
		return errors.New("runtime authentication target owner changed")
	}
	if desired != nil {
		if candidate.Providers == nil {
			return errors.New("runtime authentication candidate has no configured provider")
		}
		provider, configured := candidate.Providers.Get(target.ProviderID)
		if target.AccountNamespace == "" || desired.ID == "" || desired.AccessToken == "" || !configured || !providerHasAccount(provider, *desired) {
			return errors.New("runtime authentication account does not match candidate credentials")
		}
	} else if candidate.Providers != nil {
		if provider, configured := candidate.Providers.Get(target.ProviderID); configured && (provider.APIKey != "" || provider.APIKeyTemplate != "" || provider.OAuthToken != nil) {
			return errors.New("runtime authentication logout candidate retains credentials")
		}
	}
	return c.finalizeRuntimeAuthenticationAuthority(candidate, target, desired)
}

// The checked API key owns this credential slot while stored OAuth selection
// remains unchanged. Other captured account owners retain their exact entries.
func (c AuthenticationCapture) finalizeRuntimeCheckedAPIKey(candidate *Config, target providerregistry.RegistrationOwner) error {
	if candidate == nil || candidate == c.runtime.config || !slices.Contains(c.owners, target) {
		return errors.New("checked runtime requires an unpublished captured candidate")
	}
	provider, ok := candidate.Providers.Get(target.ProviderID)
	actual, active := candidate.ProviderOwner(target.ProviderID)
	if !ok || !active || actual != target || !provider.resolvedAPIKey.matches(provider) || provider.resolvedAPIKey.owner != target {
		return errResolvedProviderAPIKeyStale
	}
	return c.finalizeRuntimeAuthenticationAuthority(candidate, target, nil)
}

func (c AuthenticationCapture) finalizeRuntimeAuthenticationAuthority(candidate *Config, target providerregistry.RegistrationOwner, desired *accounts.Entry) error {
	next := &authenticationRuntimeAccounts{target: target, entries: make(map[providerregistry.RegistrationOwner]*accounts.Entry)}
	for _, owner := range c.owners {
		if owner.AccountNamespace == "" {
			continue
		}
		if current, ok := candidate.ProviderOwner(owner.ProviderID); !ok || current != owner {
			return errors.New("runtime authentication captured owner changed")
		}
		var selected *accounts.Entry
		activeID := c.accounts.ActiveID(owner.AccountNamespace)
		if activeID != "" {
			for _, entry := range c.accounts.Entries(owner.AccountNamespace) {
				if entry.ID == activeID {
					selected = cloneConstructionAccount(&entry)
					break
				}
			}
		}
		// A missing or dangling active ID is captured absence, never a reason
		// to choose the first account or consult a later file observation.
		next.entries[owner] = selected
	}
	if target.AccountNamespace != "" {
		next.entries[target] = cloneConstructionAccount(desired)
	}
	candidate.authenticationAccounts = next
	return nil
}

// CapturedConstructionAccount returns a detached entry or authoritative absence.
// Missing entries in a marked generation are errors, never live-account fallback.
func (s RuntimeSnapshot) CapturedConstructionAccount(owner providerregistry.RegistrationOwner) (*accounts.Entry, bool, error) {
	if s.config == nil || s.config.authenticationAccounts == nil {
		return nil, false, nil
	}
	if s.IsClientOwned() {
		return nil, true, errors.New("local construction accounts cannot authorize a client runtime")
	}
	current, ok := s.ProviderOwner(owner.ProviderID)
	if !ok || current != owner {
		return nil, true, errors.New("captured construction account owner changed")
	}
	if owner.AccountNamespace == "" {
		return nil, true, nil
	}
	entry, captured := s.config.authenticationAccounts.entries[owner]
	if !captured {
		return nil, true, errors.New("construction account owner was not captured")
	}
	return cloneConstructionAccount(entry), true, nil
}

// advanceRuntimeAuthenticationAccount is used only with an already proven
// selected-account result. Bare token writes cannot fabricate account identity.
func (c *Config) advanceRuntimeAuthenticationAccount(owner providerregistry.RegistrationOwner, entry *accounts.Entry) error {
	if c.authenticationAccounts == nil {
		return nil
	}
	if current, ok := c.ProviderOwner(owner.ProviderID); !ok || current != owner {
		return errors.New("refreshed construction account owner changed")
	}
	if _, captured := c.authenticationAccounts.entries[owner]; !captured {
		return errors.New("refreshed construction account owner was not captured")
	}
	provider, configured := c.Providers.Get(owner.ProviderID)
	if entry == nil || entry.ID == "" || !configured || !providerHasAccount(provider, *entry) {
		return errors.New("refreshed construction account does not match credentials")
	}
	next := *c.authenticationAccounts
	next.entries = maps.Clone(next.entries)
	next.entries[owner] = cloneConstructionAccount(entry)
	c.authenticationAccounts = &next
	return nil
}

// AuthenticationConstructionDenial applies only to this authentication target
// while its exact owner is still disabled. It never enables or reselects it.
func (s RuntimeSnapshot) AuthenticationConstructionDenial(providerID string) error {
	if s.config == nil || s.config.authenticationAccounts == nil || s.config.Providers == nil || s.IsClientOwned() {
		return nil
	}
	target := s.config.authenticationAccounts.target
	if target.ProviderID != providerID {
		return nil
	}
	current, ok := s.ProviderOwner(providerID)
	if !ok || current != target {
		return nil
	}
	provider, configured := s.config.Providers.Get(providerID)
	if !configured || !provider.Disable {
		return nil
	}
	return fmt.Errorf("provider %q: %w", providerID, ErrAuthenticationProviderDisabled)
}
