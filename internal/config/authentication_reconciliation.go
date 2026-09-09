package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
)

var (
	// ErrAuthenticationReconciliationReloadRequired means saved inputs have not
	// been accepted by a normal load. Re-reading status cannot accept them.
	ErrAuthenticationReconciliationReloadRequired = errors.New("reload saved configuration before reviewing authentication")
	// ErrAuthenticationReconciliationConflict means the observation is stale,
	// unprovable, or does not represent the caller's expected account effect.
	ErrAuthenticationReconciliationConflict = errors.New("saved authentication state conflicts with the requested reconciliation")
)

// AuthenticationReconciliationEffect states what already-saved state must prove.
// Logout requires an empty AccountID; a switch requires a nonempty AccountID.
// This is validation, never authorization to repeat or repair a transaction.
type AuthenticationReconciliationEffect struct {
	Logout    bool
	AccountID string
}

// AuthenticationReconciliationPreparation retains one verified current capture.
// It is host-private, immutable, and not an original mutation's success receipt.
// Later collection/publication must revalidate this capture; the preparation is
// point-in-time evidence, not a lease or permission to substitute newer inputs.
type AuthenticationReconciliationPreparation struct {
	capture     AuthenticationCapture
	owner       providerregistry.RegistrationOwner
	effect      AuthenticationReconciliationEffect
	definitions map[providerregistry.RegistrationOwner]string
	valid       bool
}

func (AuthenticationReconciliationPreparation) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication reconciliation preparations are private")
}

func (AuthenticationReconciliationPreparation) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication reconciliation preparation]"))
}

// AuthenticationCapture returns only this preparation's original observation.
// It does not reread files, resolve commands, or claim it is still current.
func (p AuthenticationReconciliationPreparation) AuthenticationCapture() (AuthenticationCapture, bool) {
	return p.capture, p.valid
}

// PrepareAuthenticationReconciliation verifies already accepted coherent saved
// state. It performs no account/config writes, reload, runtime publication,
// refresh, configured key/header/endpoint resolution, or runtime collection.
// Exact accepted shell evaluations are reused only while their raw inputs and
// publication/environment remain unchanged. A raw basis mismatch requires an
// explicit normal reload; credential disagreement requires a separate repair.
// Logout analyzes catalog fallback using only captured environment values and
// command-free parameter expansion; command-bearing fallbacks fail visibly.
func (s *ConfigStore) PrepareAuthenticationReconciliation(ctx context.Context, current AuthenticationCapture, owner providerregistry.RegistrationOwner, effect AuthenticationReconciliationEffect) (AuthenticationReconciliationPreparation, error) {
	zero := AuthenticationReconciliationPreparation{}
	if current.runtime.IsClientOwned() {
		return zero, ErrClientRuntimeManaged
	}
	if effect.Logout == (effect.AccountID != "") || owner.ProviderID == "" {
		return zero, reconciliationConflict("invalid expected effect or owner")
	}
	if current.runtime.publicationStore != s || !current.inputs.valid || !current.accounts.SameObservation(current.accounts) {
		return zero, reconciliationConflict("a valid capture from this store is required")
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return zero, err
	}
	defer s.writeMu.RUnlock()
	if err := lockAuthenticationMutex(ctx, s.configMu.TryLock, s.configMu.Unlock); err != nil {
		return zero, err
	}
	now := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.Unlock()
	if now.IsClientOwned() {
		return zero, ErrClientRuntimeManaged
	}
	expectedEnvironment, currentEnvironment := current.runtime.Environment(), now.Environment()
	slices.Sort(expectedEnvironment)
	slices.Sort(currentEnvironment)
	if !current.runtime.SamePublication(now) || !slices.Equal(expectedEnvironment, currentEnvironment) {
		return zero, reconciliationConflict("the captured publication or environment changed")
	}
	if err := s.verifyAuthenticationCollectionLocked(ctx, current); err != nil {
		return zero, reconciliationObservationError(ctx, err)
	}
	layers, err := current.authenticationReconciliationLayers()
	if err != nil {
		return zero, err
	}
	if err := current.validateConfigBasis(layers, ""); err != nil {
		return zero, fmt.Errorf("%w: accepted input evidence differs", ErrAuthenticationReconciliationReloadRequired)
	}
	if err := validateAuthenticationCandidateOwner(current, owner); err != nil {
		return zero, reconciliationConflict("the initiating complete owner is no longer active")
	}
	provider, configured := current.runtime.config.Providers.Get(owner.ProviderID)
	if effect.Logout {
		if configured && (provider.APIKey != "" || provider.APIKeyTemplate != "" || provider.OAuthToken != nil) || current.accounts.ActiveID(owner.AccountNamespace) != "" || len(current.accounts.Entries(owner.AccountNamespace)) != 0 {
			return zero, reconciliationConflict("logout still has saved credentials or accounts")
		}
		if err := validateAuthenticationLogoutFallback(ctx, current.runtime, current.runtime.config, owner); err != nil {
			return zero, reconciliationObservationError(ctx, err)
		}
		retained, _, err := current.runtime.CapturedConstructionAccount(owner)
		if err != nil || retained != nil {
			return zero, reconciliationConflict("logout retains an accepted construction account")
		}
	} else {
		if !owner.HasOAuth || owner.AccountNamespace == "" || !configured {
			return zero, reconciliationConflict("switch requires a configured OAuth owner")
		}
		if current.accounts.ActiveID(owner.AccountNamespace) != effect.AccountID {
			return zero, reconciliationConflict("the saved active account is not the requested account")
		}
		if err := current.validateReconciliationAccount(owner, provider); err != nil {
			return zero, err
		}
	}
	definitions := map[providerregistry.RegistrationOwner]string{}
	selected := map[string]bool{}
	for _, model := range current.runtime.config.Models {
		selected[model.Provider] = true
	}
	if images := current.runtime.config.Images; images != nil {
		for _, image := range images.Providers {
			for _, credential := range image.Credentials {
				selected[credential.ProviderID] = true
				actual, ok := current.runtime.ProviderOwner(credential.ProviderID)
				if !ok || actual != credential {
					return zero, reconciliationConflict("a selected image credential owner changed")
				}
			}
		}
	}
	// An unselected configured target still needs its actual current definition.
	if configured {
		selected[owner.ProviderID] = true
	}
	for id := range selected {
		definition, actual, err := current.runtime.clientProviderDefinitionRaw(id)
		if err != nil || !slices.Contains(current.owners, actual) {
			return zero, reconciliationConflict("a selected provider definition is unavailable")
		}
		digest, err := definition.Digest()
		if err != nil {
			return zero, reconciliationConflict("a selected provider definition is invalid")
		}
		definitions[actual] = digest
		value, _ := current.runtime.config.Providers.Get(id)
		if err := current.runtime.config.ValidateProviderConfiguration(id, value.Configuration); err != nil {
			return zero, reconciliationConflict("a selected provider configuration is invalid")
		}
		if id == owner.ProviderID && effect.Logout || value.Disable || errors.Is(current.runtime.AuthenticationRevocation(id), ErrAuthenticationRevoked) {
			continue
		}
		if actual.HasOAuth || value.OAuthToken != nil {
			if err := current.validateReconciliationAccount(actual, value); err != nil {
				return zero, err
			}
		}
	}
	if err := s.verifyAuthenticationCollectionLocked(ctx, current); err != nil {
		return zero, reconciliationObservationError(ctx, err)
	}
	return AuthenticationReconciliationPreparation{capture: current, owner: owner, effect: effect, definitions: definitions, valid: true}, nil
}

func (current AuthenticationCapture) authenticationReconciliationLayers() (authenticationLayers, error) {
	basis := current.runtime.config.authenticationBasis
	if basis == nil || !basis.valid {
		return authenticationLayers{}, fmt.Errorf("%w: accepted input evidence is unavailable", ErrAuthenticationReconciliationReloadRequired)
	}
	layers := authenticationLayers{order: slices.Clone(current.inputs.order), values: map[string][]byte{}}
	for _, path := range layers.order {
		file, found := current.inputs.file(path)
		accepted, known := basis.sources[path]
		if !found || !known || file.info.exists != accepted.exists {
			return authenticationLayers{}, fmt.Errorf("%w: saved input topology changed", ErrAuthenticationReconciliationReloadRequired)
		}
		if isShellConfig(path) {
			if !bytes.Equal(file.data, accepted.raw) {
				return authenticationLayers{}, fmt.Errorf("%w: saved shell input changed", ErrAuthenticationReconciliationReloadRequired)
			}
			layers.values[path] = bytes.Clone(accepted.evaluated)
		} else {
			layers.values[path] = bytes.Clone(file.data)
		}
	}
	return layers, nil
}

func (current AuthenticationCapture) validateReconciliationAccount(owner providerregistry.RegistrationOwner, provider ProviderConfig) error {
	active := current.accounts.ActiveID(owner.AccountNamespace)
	var selected *accounts.Entry
	for _, entry := range current.accounts.Entries(owner.AccountNamespace) {
		if entry.ID == active && active != "" {
			if selected != nil {
				return reconciliationConflict("a saved active account is ambiguous")
			}
			copy := entry
			selected = &copy
		}
	}
	if selected == nil {
		return reconciliationConflict("a selected provider has no saved active account")
	}
	if selected.AccessToken == "" || !providerHasAccount(provider, *selected) {
		return reconciliationConflict("saved provider credentials disagree with the active account")
	}
	retained, captured, err := current.runtime.CapturedConstructionAccount(owner)
	if err != nil || captured && (retained == nil || !reconciliationAccountsEqual(*retained, *selected)) {
		return reconciliationConflict("saved account metadata disagrees with the accepted construction account")
	}
	return nil
}

func reconciliationAccountsEqual(left, right accounts.Entry) bool {
	raw, expectedRaw := left.Raw, right.Raw
	left.Raw, right.Raw = nil, nil
	// Display names do not affect execution. Preserve the same metadata
	// number-spelling and absence/null rules as accepted-authentication checks.
	left.DisplayName = right.DisplayName
	return reflect.DeepEqual(left, right) && authenticationMetadataEqual(raw, expectedRaw)
}

func reconciliationConflict(detail string) error {
	return fmt.Errorf("%w: %s", ErrAuthenticationReconciliationConflict, detail)
}

func reconciliationObservationError(ctx context.Context, _ error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return reconciliationConflict("the captured saved state could not be verified")
}
