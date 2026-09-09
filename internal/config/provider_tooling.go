package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/example-git/crux/internal/providerregistry"
)

// ValidateProviderToolingInstructions checks every provider's effective tooling
// profile independently of instruction_mode. Call after binding the candidate's
// exact provider scan; a mode that currently omits tooling cannot admit a broken
// profile for later use. An empty field means the registration/default profile.
func (c *Config) ValidateProviderToolingInstructions() error {
	if c == nil || c.Providers == nil {
		return nil
	}
	for providerID := range c.Providers.Seq2() {
		if err := c.validateProviderToolingInstructions(providerID); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateProviderToolingInstructions(providerID string) error {
	provider, ok := c.Providers.Get(providerID)
	if !ok {
		return fmt.Errorf("provider %q is not configured", providerID)
	}
	registration, registered := c.ProviderBehaviorRegistration(providerID)
	profile := provider.ToolingInstructions
	if profile == "" {
		profile = ToolingInstructionsCrux
		if registered && registration.Instructions != nil && registration.Instructions.SelectionDefault != "" {
			profile = registration.Instructions.SelectionDefault
		}
	}
	switch profile {
	case ToolingInstructionsCrux:
		return nil
	case ToolingInstructionsNative:
		if !registered || registration.ProviderID != providerID || registration.Instructions == nil {
			return fmt.Errorf("provider %q does not provide native tooling instructions", providerID)
		}
		text := registration.Instructions.Profiles[registration.Instructions.Default]
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("provider %q has no usable default native tooling instruction text", providerID)
		}
		return nil
	default:
		return fmt.Errorf("unsupported tooling instruction profile %q for provider %q", profile, providerID)
	}
}

func (s *ConfigStore) SetProviderToolingInstructions(scope Scope, owner providerregistry.RegistrationOwner, profile string) error {
	return s.SetProviderToolingInstructionsContext(context.Background(), scope, owner, profile)
}

func (s *ConfigStore) SetProviderToolingInstructionsContext(ctx context.Context, scope Scope, owner providerregistry.RegistrationOwner, profile string) error {
	return s.mutateProviderToolingInstructions(ctx, scope, owner, &profile)
}

// RemoveProviderToolingInstructions deletes this scope's key and reloads the
// underlying layers. Its effective result can be an inherited explicit profile
// or an empty field selecting the registration/default profile.
func (s *ConfigStore) RemoveProviderToolingInstructions(scope Scope, owner providerregistry.RegistrationOwner) error {
	return s.RemoveProviderToolingInstructionsContext(context.Background(), scope, owner)
}

func (s *ConfigStore) RemoveProviderToolingInstructionsContext(ctx context.Context, scope Scope, owner providerregistry.RegistrationOwner) error {
	return s.mutateProviderToolingInstructions(ctx, scope, owner, nil)
}

func (s *ConfigStore) mutateProviderToolingInstructions(ctx context.Context, scope Scope, owner providerregistry.RegistrationOwner, profile *string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.RemoteAuthority() != nil {
		return ErrClientRuntimeManaged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current := s.Config()
	if err := s.validateActiveProviderOwnerLocked(current, owner); err != nil {
		return err
	}
	if profile != nil {
		if *profile != ToolingInstructionsCrux && *profile != ToolingInstructionsNative {
			return fmt.Errorf("tooling instruction profile must be crux or native")
		}
		candidate := current.cloneForWrite()
		provider, _ := candidate.Providers.Get(owner.ProviderID)
		provider.ToolingInstructions = *profile
		candidate.Providers.Set(owner.ProviderID, provider)
		if err := candidate.validateProviderToolingInstructions(owner.ProviderID); err != nil {
			return err
		}
	}
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	if s.workingDir == "" {
		return errors.New("cannot update provider tooling instructions without a working directory")
	}
	before, err := os.ReadFile(path)
	existed := err == nil
	if errors.Is(err, os.ErrNotExist) {
		before = []byte("{}")
	} else if err != nil {
		return fmt.Errorf("read tooling instruction config: %w", err)
	}
	after, err := providerToolingConfigChange(before, owner.ProviderID, profile)
	if err != nil {
		return err
	}
	base := s.baseEnvironment
	if base == nil {
		base = snapshotEnvironment()
	}
	dataDir := ""
	if current.Options != nil {
		dataDir = current.Options.DataDirectory
	}
	preview, err := s.previewScopedConfigWrite(ctx, path, before, existed, after, base, dataDir)
	if err != nil {
		return fmt.Errorf("stage tooling instruction config: %w", err)
	}
	if current.providerScan != nil {
		preview.bindProviderScan(*current.providerScan)
	} else {
		preview.bindProviderScan(ProviderScan{Registry: s.providerRegistry})
	}
	if err := s.validateActiveProviderOwnerLocked(preview, owner); err != nil {
		return fmt.Errorf("staged tooling instruction owner: %w", err)
	}
	if err := preview.validateProviderToolingInstructions(owner.ProviderID); err != nil {
		return err
	}
	previewProvider, _ := preview.Providers.Get(owner.ProviderID)
	if profile != nil && previewProvider.ToolingInstructions != *profile {
		return fmt.Errorf("%s tooling instruction setting is shadowed by another configuration layer", scope)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.atomicWrite(scope, func(actual []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !bytes.Equal(actual, before) {
			return nil, errors.New("tooling instruction config changed before persistence")
		}
		return after, nil
	}); err != nil {
		return err
	}
	if err := s.reloadFromDiskLocked(ctx); err != nil {
		return fmt.Errorf("tooling instruction setting saved but runtime reload failed: %w", err)
	}
	if err := s.validateActiveProviderOwnerLocked(s.Config(), owner); err != nil {
		return fmt.Errorf("tooling instruction setting saved but owner changed during reload: %w", err)
	}
	provider, _ := s.Config().Providers.Get(owner.ProviderID)
	if provider.ToolingInstructions != previewProvider.ToolingInstructions {
		return errors.New("tooling instruction setting saved but effective configuration changed during reload")
	}
	return s.Config().validateProviderToolingInstructions(owner.ProviderID)
}

func providerToolingConfigChange(data []byte, providerID string, profile *string) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || root == nil {
		return nil, errors.New("tooling instruction config must be a JSON object")
	}
	providers := map[string]json.RawMessage{}
	if raw, ok := root["providers"]; ok {
		if err := json.Unmarshal(raw, &providers); err != nil || providers == nil {
			return nil, errors.New("tooling instruction providers must be a JSON object")
		}
	}
	provider := map[string]json.RawMessage{}
	if raw, ok := providers[providerID]; ok {
		if err := json.Unmarshal(raw, &provider); err != nil || provider == nil {
			return nil, errors.New("tooling instruction provider must be a JSON object")
		}
	} else if profile == nil {
		return bytes.Clone(data), nil
	}
	if profile == nil {
		delete(provider, "tooling_instructions")
	} else {
		provider["tooling_instructions"], _ = json.Marshal(*profile)
	}
	providers[providerID], _ = json.Marshal(provider)
	root["providers"], _ = json.Marshal(providers)
	return json.Marshal(root)
}
