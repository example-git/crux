package config

import (
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/providerregistry"
)

// ErrAuthenticationRevoked identifies a provider explicitly logged out in this
// process. It is distinct from an ordinary missing or invalid credential.
var ErrAuthenticationRevoked = errors.New("provider authentication was logged out")

// WithAuthenticationRevocation marks a prepared logout configuration after its
// scoped credential removal has been resolved. The private marker survives
// copy-on-write publication but cannot be supplied through config JSON or RPC.
// It applies only to this exact owner while all effective credentials are absent.
func (c *Config) WithAuthenticationRevocation(owner providerregistry.RegistrationOwner) (*Config, error) {
	if c == nil || c.Providers == nil || owner.ProviderID == "" {
		return nil, fmt.Errorf("logout provider owner is required")
	}
	current, ok := c.ProviderOwner(owner.ProviderID)
	if !ok || current != owner {
		return nil, fmt.Errorf("logout provider %q exact owner is unavailable", owner.ProviderID)
	}
	provider, ok := c.Providers.Get(owner.ProviderID)
	if !ok {
		return nil, fmt.Errorf("logout provider %q is not configured", owner.ProviderID)
	}
	if provider.APIKey != "" || provider.APIKeyTemplate != "" || provider.OAuthToken != nil {
		return nil, fmt.Errorf("logout provider %q still has effective credentials", owner.ProviderID)
	}
	clone := c.cloneForWrite()
	if clone.authenticationRevocations == nil {
		clone.authenticationRevocations = make(map[string]providerregistry.RegistrationOwner)
	}
	clone.authenticationRevocations[owner.ProviderID] = owner
	return clone, nil
}

// AuthenticationRevocation reports only an explicit, still-applicable logout.
// A replacement owner or a newly configured credential cannot inherit denial.
// Disabled providers remain eligible so logout can publish their cleanup without
// constructing an authenticated provider or altering the selected model state.
func (s RuntimeSnapshot) AuthenticationRevocation(providerID string) error {
	if s.config == nil || s.config.Providers == nil {
		return nil
	}
	expected, marked := s.config.authenticationRevocations[providerID]
	if !marked {
		return nil
	}
	current, ok := s.ProviderOwner(providerID)
	if !ok || current != expected {
		return nil
	}
	provider, ok := s.config.Providers.Get(providerID)
	if !ok || provider.APIKey != "" || provider.APIKeyTemplate != "" || provider.OAuthToken != nil {
		return nil
	}
	return fmt.Errorf("provider %q: %w; sign in again before starting a new request", providerID, ErrAuthenticationRevoked)
}

// retainAuthenticationRevocations carries only still-applicable local logout
// intent into an unpublished reload candidate after owner and credential
// resolution. A successful reload with a new credential or owner retires it.
func (c *Config) retainAuthenticationRevocations(previous *Config) {
	if previous == nil {
		return
	}
	for providerID, owner := range previous.authenticationRevocations {
		if !previous.authenticationRevocationApplies(owner) || !c.authenticationRevocationApplies(owner) {
			continue
		}
		if c.authenticationRevocations == nil {
			c.authenticationRevocations = make(map[string]providerregistry.RegistrationOwner)
		}
		c.authenticationRevocations[providerID] = owner
	}
}

func (c *Config) authenticationRevocationApplies(owner providerregistry.RegistrationOwner) bool {
	if c == nil || c.Providers == nil || owner.ProviderID == "" {
		return false
	}
	current, ok := c.ProviderOwner(owner.ProviderID)
	if !ok || current != owner {
		return false
	}
	provider, ok := c.Providers.Get(owner.ProviderID)
	return ok && provider.APIKey == "" && provider.APIKeyTemplate == "" && provider.OAuthToken == nil
}
