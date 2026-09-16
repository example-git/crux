package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// RefreshSelectedOAuthAccount durably rotates the owning client's exact account
// and then conditionally updates its provider config. Publication to a remote
// workspace is a separate, acknowledged transaction by the caller.
func (s *ConfigStore) RefreshSelectedOAuthAccount(ctx context.Context, scope Scope, owner providerregistry.RegistrationOwner, expected accounts.Entry, force bool) (*accounts.Entry, error) {
	return s.RefreshSelectedOAuthAccountForRuntime(ctx, scope, owner, expected, force, s.RuntimeSnapshot())
}

// RefreshSelectedOAuthAccountForRuntime preserves the local generation that a
// client validated against its accepted remote provider definition.
func (s *ConfigStore) RefreshSelectedOAuthAccountForRuntime(ctx context.Context, scope Scope, owner providerregistry.RegistrationOwner, expected accounts.Entry, force bool, admitted RuntimeSnapshot) (*accounts.Entry, error) {
	if s.RemoteAuthority() != nil {
		return nil, ErrClientRuntimeManaged
	}
	if owner.ProviderID == "" || owner.AccountNamespace == "" || expected.ID == "" {
		return nil, errors.New("refresh requires the selected account and provider owner")
	}
	definition, admittedOwner, err := admitted.clientProviderDefinitionRaw(owner.ProviderID)
	if err != nil || admittedOwner != owner {
		return nil, errors.New("selected provider definition is unavailable in the captured runtime")
	}
	validateDefinition := func(snapshot RuntimeSnapshot) error {
		actual, actualOwner, err := snapshot.clientProviderDefinitionRaw(owner.ProviderID)
		if err != nil || actualOwner != owner || !reflect.DeepEqual(actual, definition) || !snapshot.nativeIdentities.matches(admitted.Environment()) {
			return errors.New("selected provider definition changed during refresh")
		}
		return nil
	}
	lockCtx, cancel := context.WithTimeout(ctx, refreshLockDeadline)
	defer cancel()
	release, err := lock.File(lockCtx, s.refreshLockPath(owner.ProviderID))
	if err != nil {
		return nil, fmt.Errorf("acquire selected provider refresh lock: %w", err)
	}
	defer release()

	// Config generations are immutable. This validator must not acquire writeMu:
	// accounts invokes it under its lock, and config reloads read accounts while
	// holding writeMu.
	validate := func() error {
		cfg := s.Config()
		current, ok := cfg.ProviderOwner(owner.ProviderID)
		provider, exists := cfg.Providers.Get(owner.ProviderID)
		if !ok || current != owner || !exists || provider.Disable {
			return errors.New("selected provider owner changed during refresh")
		}
		return nil
	}
	if err := validate(); err != nil {
		return nil, err
	}
	currentRuntime, err := s.captureRemoteCollectionRuntime(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateDefinition(currentRuntime); err != nil {
		return nil, err
	}
	accountState, err := captureRuntimeAccounts(ctx, admitted, []string{owner.AccountNamespace})
	if err != nil {
		return nil, err
	}
	cfg := admitted.Config()
	before, _ := cfg.Providers.Get(owner.ProviderID)
	registration, ok := cfg.ProviderBehaviorRegistration(owner.ProviderID)
	if !ok || !owner.Matches(registration) || registration.OAuth == nil || registration.OAuth.Refresh == nil {
		return nil, errors.New("selected provider does not support OAuth refresh")
	}
	path, err := s.configPath(scope)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(data) == 0 {
		data = []byte("{}")
	}
	field := "providers." + owner.ProviderID
	diskBefore := gjson.GetBytes(data, field)
	if !gjson.ValidBytes(data) {
		return nil, errors.New("provider config is not valid JSON")
	}
	for key, expected := range map[string]any{"owner": before.Owner, "plugin": before.Plugin, "preset": before.Preset} {
		stored := diskBefore.Get(key)
		if stored.Exists() {
			encoded, err := json.Marshal(expected)
			if err != nil || !reflect.DeepEqual(stored.Value(), gjson.ParseBytes(encoded).Value()) {
				return nil, errors.New("selected provider owner changed on disk before refresh")
			}
		}
	}
	refresh := func(exchangeCtx context.Context, token string) (*oauth.Token, error) {
		// Accounts releases its store lock before the exchange callback. Capture
		// the full current runtime here; the lightweight owner validator below
		// remains safe when accounts invokes it under its own lock.
		currentRuntime, err := s.captureRemoteCollectionRuntime(exchangeCtx)
		if err != nil {
			return nil, err
		}
		if err := validateDefinition(currentRuntime); err != nil {
			return nil, err
		}
		// A mismatch may be a completed peer rotation, which the accounts layer
		// can adopt without entering this callback. Never exchange on a guess.
		if !providerHasAccount(before, expected) {
			return nil, fmt.Errorf("provider configuration no longer matches the selected account: %w", accounts.ErrCredentialChanged)
		}
		if !diskHasAccountOrAbsent(diskBefore, expected) {
			return nil, fmt.Errorf("provider configuration on disk no longer matches the selected account (%s): %w", diskAccountMismatchDetail(diskBefore, expected), accounts.ErrCredentialChanged)
		}
		exchangeCtx = oauth.ContextWithEnvironment(exchangeCtx, admitted.Environment())
		if s.exchangeToken != nil {
			return s.exchangeToken(exchangeCtx, owner.ProviderID, token)
		}
		return registration.OAuth.Refresh(exchangeCtx, token)
	}
	fresh, err := accountState.RefreshSelectedForOwner(ctx, owner.AccountNamespace, &expected, refresh, validate, force)
	if err != nil {
		if fresh != nil {
			return fresh, fmt.Errorf("account token saved; provider config was not updated: %w", err)
		}
		return nil, err
	}
	// The exchange may already have consumed the old refresh token. Complete
	// bounded local persistence even if the remote request disconnected.
	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancelCommit()
	err = func() error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		if err := validate(); err != nil {
			return err
		}
		s.configMu.Lock()
		currentRuntime := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
		s.configMu.Unlock()
		if err := validateDefinition(currentRuntime); err != nil {
			return err
		}
		current := currentRuntime.Config()
		provider, _ := current.Providers.Get(owner.ProviderID)
		if !reflect.DeepEqual(provider, before) && !providerHasAccount(provider, *fresh) || !providerHasAccount(before, expected) && !providerHasAccount(before, *fresh) {
			return accounts.ErrCredentialChanged
		}
		if !diskHasAccountOrAbsent(diskBefore, expected) && !diskHasAccountOrAbsent(diskBefore, *fresh) {
			return accounts.ErrCredentialChanged
		}
		applyOAuthTokenToProvider(&provider, fresh.Token(), registration)
		next := current.cloneForWrite()
		next.Providers.Set(owner.ProviderID, provider)
		if err := next.advanceRuntimeAuthenticationAccount(owner, fresh); err != nil {
			return err
		}
		fields := map[string]any{"api_key": fresh.AccessToken, "oauth": fresh.Token()}
		if provider.Owner != nil {
			fields["owner"] = provider.Owner
		}
		if provider.Plugin != nil {
			fields["plugin"] = provider.Plugin
		} else if provider.Preset != nil {
			fields["preset"] = provider.Preset
		}
		authored := make(map[string]any, len(fields))
		for key, value := range fields {
			authored[field+"."+key] = value
		}
		written, err := s.prepareAuthenticationCOW(commitCtx, next, path, authored, nil)
		if err != nil {
			return err
		}
		return accountState.WithSelectedForOwner(commitCtx, owner.AccountNamespace, *fresh, validate, func() error {
			if err := s.atomicWrite(scope, func(data []byte) ([]byte, error) {
				if !gjson.ValidBytes(data) || !reflect.DeepEqual(gjson.GetBytes(data, field).Value(), diskBefore.Value()) {
					return nil, accounts.ErrCredentialChanged
				}
				for key, value := range fields {
					var err error
					data, err = sjson.SetBytes(data, field+"."+key, value)
					if err != nil {
						return nil, err
					}
				}
				return data, nil
			}); err != nil {
				return err
			}
			if err := s.verifyAuthenticationCOW(commitCtx, next, written); err != nil {
				return fmt.Errorf("provider config saved but failed to publish in-memory state: %w", err)
			}
			s.captureStalenessSnapshot(append(slices.Clone(s.loadedPaths), path))
			s.setConfig(next)
			return nil
		})
	}()
	if err != nil {
		return fresh, fmt.Errorf("account token saved; provider config was not updated: %w", err)
	}
	return fresh, nil
}

func providerHasAccount(provider ProviderConfig, entry accounts.Entry) bool {
	// Declarative OAuth providers may retain only the selected access token in
	// api_key. The exact selected account and rotation are checked separately by
	// the account store. If oauth is present, all of its identity fields must
	// still agree; absence must not defeat a collector-admitted account.
	return provider.APIKey == entry.AccessToken && (provider.OAuthToken == nil || tokenHasAccount(provider.OAuthToken, entry))
}

func tokenHasAccount(token *oauth.Token, entry accounts.Entry) bool {
	return token != nil && token.AccessToken == entry.AccessToken && token.RefreshToken == entry.RefreshToken && token.ExpiresAt == entry.Token().ExpiresAt
}

func diskHasAccountOrAbsent(provider gjson.Result, entry accounts.Entry) bool {
	key, token := provider.Get("api_key"), provider.Get("oauth")
	if key.Exists() && key.String() != entry.AccessToken {
		return false
	}
	if token.Exists() {
		var stored oauth.Token
		if err := json.Unmarshal([]byte(token.Raw), &stored); err != nil || !tokenHasAccount(&stored, entry) {
			return false
		}
	}
	return true
}

// diskAccountMismatchDetail explains, without revealing any token value, why
// diskHasAccountOrAbsent rejected the disk-persisted provider configuration
// against the expected account. It exists purely for diagnostics: the exact
// mechanism differentiating this failure from a genuine credential change
// (stale on-disk copy vs. a real rotation) is otherwise invisible once the
// error is generic.
func diskAccountMismatchDetail(provider gjson.Result, entry accounts.Entry) string {
	key, token := provider.Get("api_key"), provider.Get("oauth")
	if key.Exists() && key.String() != entry.AccessToken {
		return "disk api_key field does not match the selected account's access token"
	}
	if !token.Exists() {
		return "disk provider configuration has neither a matching api_key nor an oauth field"
	}
	var stored oauth.Token
	if err := json.Unmarshal([]byte(token.Raw), &stored); err != nil {
		return "disk oauth field is not a valid stored token"
	}
	expectedToken := entry.Token()
	switch {
	case stored.AccessToken != entry.AccessToken:
		return "disk oauth access_token does not match the selected account"
	case stored.RefreshToken != entry.RefreshToken:
		return "disk oauth refresh_token does not match the selected account"
	case stored.ExpiresAt != expectedToken.ExpiresAt:
		return "disk oauth expires_at does not match the selected account"
	default:
		return "disk oauth token does not match the selected account for an undetermined field"
	}
}
