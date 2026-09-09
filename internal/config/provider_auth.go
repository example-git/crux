package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerregistry"
)

// AuthenticationCapture is host-private. It retains one accepted configuration
// publication and a coherent account-file observation. Never send it over RPC.
type AuthenticationCapture struct {
	runtime  RuntimeSnapshot
	accounts accounts.Snapshot
	owners   []providerregistry.RegistrationOwner
	inputs   authenticationConfigInputs
}

func (AuthenticationCapture) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication captures are private")
}
func (AuthenticationCapture) String() string   { return "[private authentication capture]" }
func (AuthenticationCapture) GoString() string { return "[private authentication capture]" }
func (AuthenticationCapture) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication capture]"))
}

// AuthenticationProvider and AuthenticationAccount contain no credential
// material. Owner still includes the private account namespace; the auth
// service converts it to its public reference before returning a response.
type AuthenticationProvider struct {
	Owner                         providerregistry.RegistrationOwner
	Configured, Disabled          bool
	APIKeyConfigured              bool
	OAuthState                    string
	OAuthRefreshable              bool
	AccountState, ActiveAccountID string
}

type AuthenticationAccount struct {
	ID, DisplayName string
	Active          bool
	CredentialState string
	Refreshable     bool
}

func (s *ConfigStore) CaptureAuthentication(ctx context.Context) (AuthenticationCapture, error) {
	// A cancelable read-lock wait avoids holding up workspace teardown behind a
	// long-running configuration writer. Lock ordering remains config->accounts.
	for !s.writeMu.TryRLock() {
		select {
		case <-ctx.Done():
			return AuthenticationCapture{}, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	defer s.writeMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return AuthenticationCapture{}, err
	}
	s.configMu.Lock()
	runtime := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.Unlock()
	// This guard must precede path lookup, mkdir, account-lock creation and reads.
	if runtime.IsClientOwned() {
		return AuthenticationCapture{}, ErrClientRuntimeManaged
	}
	if runtime.Config() == nil {
		return AuthenticationCapture{}, errors.New("authentication configuration is unavailable")
	}
	ids := map[string]bool{}
	if runtime.registry != nil {
		for _, r := range runtime.registry.Registrations() {
			ids[r.ProviderID] = true
		}
	}
	if runtime.config.providerScan != nil {
		for _, p := range runtime.config.providerScan.Providers {
			ids[string(p.ID)] = true
		}
		for id := range runtime.config.providerScan.presetReferences {
			ids[id] = true
		}
	}
	if runtime.config.Providers != nil {
		for id := range runtime.config.Providers.Seq2() {
			ids[id] = true
		}
	}
	capture := AuthenticationCapture{runtime: runtime}
	namespaces := []string{}
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		owner, ok := runtime.ProviderOwner(id)
		if !ok {
			continue
		} // No live owner may route another registration's accounts.
		capture.owners = append(capture.owners, owner)
		if owner.AccountNamespace != "" {
			namespaces = append(namespaces, owner.AccountNamespace)
		}
	}
	root := runtime.Getenv("AI_CLI_DIR")
	if root == "" {
		home := runtime.Getenv(authenticationHomeVariable())
		if !filepath.IsAbs(home) {
			return AuthenticationCapture{}, errors.New("captured account home is unavailable or not absolute")
		}
		root = filepath.Join(home, ".ai-cli")
	}
	if !filepath.IsAbs(root) {
		return AuthenticationCapture{}, errors.New("captured account directory is not absolute")
	}
	state, err := accounts.CaptureStateAt(ctx, filepath.Join(root, "accounts.json"), namespaces)
	if err != nil {
		if ctx.Err() != nil {
			return AuthenticationCapture{}, ctx.Err()
		}
		// File paths, JSON parse details, and account data never enter public errors.
		return AuthenticationCapture{}, errors.New("authentication account store cannot be read")
	}
	inputs, err := s.captureAuthenticationInputsLocked(ctx, runtime)
	if err != nil {
		return AuthenticationCapture{}, err
	}
	second, err := accounts.CaptureStateAt(ctx, filepath.Join(root, "accounts.json"), namespaces)
	if err != nil {
		if ctx.Err() != nil {
			return AuthenticationCapture{}, ctx.Err()
		}
		return AuthenticationCapture{}, errors.New("authentication account store cannot be read")
	}
	finalInputs, err := s.captureAuthenticationInputsLocked(ctx, runtime)
	if err != nil {
		return AuthenticationCapture{}, err
	}
	if err := validateAuthenticationObservations(state, second, inputs, finalInputs); err != nil {
		return AuthenticationCapture{}, err
	}
	capture.accounts, capture.inputs = second, inputs
	if err := ctx.Err(); err != nil {
		return AuthenticationCapture{}, err
	}
	return capture, nil
}

func authenticationHomeVariable() string {
	if runtime.GOOS == "windows" {
		return "USERPROFILE"
	}
	return "HOME"
}

func (c AuthenticationCapture) SameObservation(other AuthenticationCapture) bool {
	return c.runtime.SamePublication(other.runtime) && c.accounts.SameObservation(other.accounts) && c.inputs.sameObservation(other.inputs) && slices.Equal(c.owners, other.owners)
}

func authenticationCredentialState(access, refresh string) string {
	if access != "" {
		return "present"
	}
	if refresh != "" {
		return "refresh-only"
	}
	return "absent"
}

func (c AuthenticationCapture) Providers() []AuthenticationProvider {
	result := make([]AuthenticationProvider, 0, len(c.owners))
	for _, owner := range c.owners {
		p := AuthenticationProvider{Owner: owner, OAuthState: "absent", AccountState: "none"}
		var provider ProviderConfig
		var configured bool
		if c.runtime.config.Providers != nil {
			provider, configured = c.runtime.config.Providers.Get(owner.ProviderID)
		}
		p.Configured, p.Disabled, p.APIKeyConfigured = configured, provider.Disable, provider.APIKey != "" || provider.APIKeyTemplate != ""
		if token := provider.OAuthToken; token != nil {
			p.OAuthState = authenticationCredentialState(token.AccessToken, token.RefreshToken)
			p.OAuthRefreshable = token.RefreshToken != ""
		}
		if owner.AccountNamespace != "" {
			p.ActiveAccountID = c.accounts.ActiveID(owner.AccountNamespace)
			if p.ActiveAccountID != "" || provider.OAuthToken != nil || owner.HasOAuth && p.APIKeyConfigured {
				p.AccountState = "out-of-sync"
				for _, entry := range c.accounts.Entries(owner.AccountNamespace) {
					if entry.ID == p.ActiveAccountID && entry.ID != "" && configured && providerHasAccount(provider, entry) {
						p.AccountState = "in-sync"
					}
				}
			}
		}
		result = append(result, p)
	}
	return result
}

func (c AuthenticationCapture) Accounts(owner providerregistry.RegistrationOwner) ([]AuthenticationAccount, error) {
	if !slices.Contains(c.owners, owner) {
		return nil, errors.New("authentication account owner changed")
	}
	result := []AuthenticationAccount{}
	if owner.AccountNamespace == "" {
		return result, nil
	}
	active := c.accounts.ActiveID(owner.AccountNamespace)
	for _, entry := range c.accounts.Entries(owner.AccountNamespace) {
		result = append(result, AuthenticationAccount{ID: entry.ID, DisplayName: entry.DisplayName, Active: entry.ID == active, CredentialState: authenticationCredentialState(entry.AccessToken, entry.RefreshToken), Refreshable: entry.RefreshToken != ""})
	}
	slices.SortFunc(result, func(a, b AuthenticationAccount) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return result, nil
}

// ValidateAcceptedAuthentication compares this same private capture against the
// owning client's retained acknowledged config/proposal. No shell expansion,
// provider request, disk reread, runtime collection or publication occurs here.
func (c AuthenticationCapture) ValidateAcceptedAuthentication(accepted RemoteRuntimeProposal, view *Config) error {
	pending := errors.New("local authentication changes are not acknowledged by the execution host; publish or reload the client runtime")
	if c.runtime.config == nil || view == nil || accepted.Revision == 0 || accepted.Digest == "" {
		return pending
	}
	// Compare all configured provider records, including unselected credentials.
	// A successful auth read must not conceal saved but unacknowledged edits.
	currentJSON, err := json.Marshal(c.runtime.config.Providers)
	if err != nil {
		return pending
	}
	viewJSON, err := json.Marshal(view.Providers)
	if err != nil || !RuntimeControlJSONEqual(currentJSON, viewJSON) {
		return pending
	}
	modelsJSON, err := json.Marshal(c.runtime.config.Models)
	if err != nil {
		return pending
	}
	acceptedModels, err := json.Marshal(accepted.Models)
	if err != nil || !RuntimeControlJSONEqual(modelsJSON, acceptedModels) {
		return pending
	}
	// Collection provenance binds unresolved expressions to the exact source
	// publication that produced the acknowledged resolved credentials. Status
	// must not execute those expressions a second time.
	source := accepted.collectionSource
	if source != nil && source.digest != accepted.Digest {
		return pending
	}
	if source != nil {
		if source.runtime.publicationStore != c.runtime.publicationStore {
			return pending
		}
		currentEnv, sourceEnv := c.runtime.Environment(), source.runtime.Environment()
		slices.Sort(currentEnv)
		slices.Sort(sourceEnv)
		if !slices.Equal(currentEnv, sourceEnv) {
			return pending
		}
	}
	for _, transported := range accepted.Providers {
		current, _, err := c.runtime.clientProviderDefinitionRaw(transported.Config.ID)
		if err != nil {
			return pending
		}
		wanted := transported
		if source != nil {
			wanted, _, err = source.runtime.clientProviderDefinitionRaw(transported.Config.ID)
			if err != nil {
				return pending
			}
		} else if current.Config.BaseURL == "" && current.Config.Owner != nil && current.Config.Owner.Type == ProviderOwnerCore {
			switch current.Config.Owner.Construction {
			case providerregistry.ConstructionCodex:
				current.Config.BaseURL = codex.APIEndpoint
			case providerregistry.ConstructionGeminiAntigravity:
				current.Config.BaseURL = gemini.APIEndpoint
			}
		}
		if transported.Config.Owner != nil && nativeConstruction(transported.Config.Owner.Construction) {
			capture := c.runtime.nativeIdentities
			if source != nil {
				capture = source.runtime.nativeIdentities
			}
			identity, ready := capture.peek(transported.Config.Owner.Construction)
			if !ready || transported.NativeIdentity == nil || identity != *transported.NativeIdentity {
				return pending
			}
		}
		// Raw comparisons are immutable input projections; the exact resolved
		// declaration was checked separately against its retained capture above.
		wanted.NativeIdentity = nil
		currentJSON, err := json.Marshal(current)
		if err != nil {
			return pending
		}
		wantedJSON, err := json.Marshal(wanted)
		if err != nil || !RuntimeControlJSONEqual(currentJSON, wantedJSON) {
			return pending
		}
		// Even with local provenance the immutable admitted bundle binding must
		// agree with the current executable definition.
		if current.BundleDigest != transported.BundleDigest {
			return pending
		}
	}
	for _, binding := range accepted.Credentials {
		owner, ok := c.runtime.ProviderOwner(binding.Owner.ProviderID)
		if !ok || owner != binding.Owner {
			return pending
		}
		provider, ok := c.runtime.config.Providers.Get(owner.ProviderID)
		if !ok {
			return pending
		}
		if binding.Unavailable { // Acknowledged logout/disable has no executable credential.
			if !provider.Disable && (provider.APIKey != "" || provider.OAuthToken != nil) {
				return pending
			}
			continue
		}
		if binding.Account != nil {
			if c.accounts.ActiveID(owner.AccountNamespace) != binding.Account.ID || !providerHasAccount(provider, *binding.Account) {
				return pending
			}
			found := false
			for _, entry := range c.accounts.Entries(owner.AccountNamespace) {
				if entry.ID == binding.Account.ID {
					// Display names are local presentation. Every credential/metadata
					// field that can affect execution must match the accepted entry.
					entry.DisplayName = binding.Account.DisplayName
					wanted := *binding.Account
					raw, wantedRaw := entry.Raw, wanted.Raw
					entry.Raw, wanted.Raw = nil, nil
					found = reflect.DeepEqual(entry, wanted) && authenticationMetadataEqual(raw, wantedRaw)
				}
			}
			if !found {
				return pending
			}
		} else if provider.APIKey != binding.APIKey {
			if source == nil {
				return pending
			}
			collected, ok := source.runtime.config.Providers.Get(owner.ProviderID)
			if !ok || collected.APIKey != provider.APIKey || collected.APIKeyTemplate != provider.APIKeyTemplate {
				return pending
			}
		}
	}
	return nil
}

// Account metadata can participate in declared image credentials. Its number
// spelling and absence/null distinction affect execution, unlike control values.
func authenticationMetadataEqual(left, right json.RawMessage) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	var a, b any
	da, db := json.NewDecoder(bytes.NewReader(left)), json.NewDecoder(bytes.NewReader(right))
	da.UseNumber()
	db.UseNumber()
	return da.Decode(&a) == nil && db.Decode(&b) == nil && reflect.DeepEqual(a, b)
}
