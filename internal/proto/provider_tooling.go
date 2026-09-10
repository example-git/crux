package proto

import (
	"fmt"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
)

// ProviderToolingRequest sets one exact owner's tooling profile in an explicit scope.
type ProviderToolingRequest struct {
	Scope   *config.Scope                      `json:"scope"`
	Owner   providerregistry.RegistrationOwner `json:"owner"`
	Profile string                             `json:"profile"`
}

// RemoveProviderToolingRequest removes the scoped value, allowing inheritance.
type RemoveProviderToolingRequest struct {
	Scope *config.Scope                      `json:"scope"`
	Owner providerregistry.RegistrationOwner `json:"owner"`
}

// ProviderToolingState reports the effective field after a scoped mutation.
// An empty profile selects the provider's default instructions.
type ProviderToolingState struct {
	Scope   config.Scope                       `json:"scope"`
	Owner   providerregistry.RegistrationOwner `json:"owner"`
	Profile string                             `json:"profile"`
}

func ValidateProviderToolingRequest(scope *config.Scope, owner providerregistry.RegistrationOwner, profile string, remove bool) error {
	if scope == nil || (*scope != config.ScopeGlobal && *scope != config.ScopeWorkspace) {
		return fmt.Errorf("provider tooling scope must be global or workspace")
	}
	if owner.ProviderID == "" {
		return fmt.Errorf("provider tooling owner is required")
	}
	if profile != config.ToolingInstructionsCrux && profile != config.ToolingInstructionsNative && (!remove || profile != "") {
		return fmt.Errorf("provider tooling profile must be crux or native")
	}
	return nil
}

// ValidateConfig ensures a refreshed configuration still represents this acknowledgement.
func (s ProviderToolingState) ValidateConfig(cfg *config.Config) error {
	if cfg == nil || cfg.Providers == nil {
		return fmt.Errorf("provider tooling configuration is unavailable")
	}
	owner, ok := cfg.ProviderOwner(s.Owner.ProviderID)
	provider, configured := cfg.Providers.Get(s.Owner.ProviderID)
	if !ok || owner != s.Owner || !configured || provider.Disable {
		return fmt.Errorf("provider tooling owner changed before acknowledgement")
	}
	if provider.ToolingInstructions != s.Profile {
		return fmt.Errorf("provider tooling profile changed before acknowledgement")
	}
	return nil
}
