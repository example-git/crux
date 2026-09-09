package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ImportCopilotForOwner imports from this store's captured environment and
// commits a coherent selected local account/config. A receiver cannot execute
// this operation for a client-owned runtime, including its disk-read phase.
func (s *ConfigStore) ImportCopilotForOwner(ctx context.Context, owner providerregistry.RegistrationOwner) (*oauth.Token, bool, error) {
	snapshot := s.RuntimeSnapshot()
	if snapshot.IsClientOwned() {
		return nil, false, ErrClientRuntimeManaged
	}
	registration, ok := snapshot.ProviderRegistration(owner.ProviderID)
	if !ok || registration.Owner() != owner || owner.ProviderID != "copilot" || registration.Construction != providerregistry.ConstructionCopilot || registration.OAuth == nil || registration.OAuth.Import == nil || registration.AccountNamespace == "" {
		return nil, false, errors.New("Copilot import requires its active initiating owner")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	before, configured := snapshot.Config().Providers.Get(owner.ProviderID)
	if configured && (before.APIKey != "" || before.OAuthToken != nil) {
		return nil, false, nil
	}
	path, err := s.configPath(ScopeGlobal)
	if err != nil {
		return nil, false, err
	}
	preimage, err := readAuthenticationInput(ctx, path)
	if err != nil {
		return nil, false, err
	}
	data := preimage.data
	if len(data) == 0 {
		data = []byte("{}")
	}
	if !gjson.ValidBytes(data) {
		return nil, false, errors.New("provider config is not valid JSON")
	}
	field := "providers." + owner.ProviderID
	diskBefore := gjson.GetBytes(data, field)
	if diskBefore.Get("api_key").Exists() || diskBefore.Get("oauth").Exists() {
		return nil, false, nil
	}
	// Do not overwrite an ownership edit made outside this store before import.
	expectedOwner := providerOwnerReferenceForRegistration(registration)
	expectedPlugin := before.Plugin
	if registration.Manifest != nil {
		expectedPlugin = &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}
	}
	for key, expected := range map[string]any{"owner": expectedOwner, "plugin": expectedPlugin, "preset": before.Preset} {
		if stored := diskBefore.Get(key); stored.Exists() {
			encoded, err := json.Marshal(expected)
			if err != nil || !reflect.DeepEqual(stored.Value(), gjson.ParseBytes(encoded).Value()) {
				return nil, false, errors.New("provider owner changed on disk before import")
			}
		}
	}
	validate := func() error {
		current := s.Config()
		active, ok := current.ProviderRegistration(owner.ProviderID)
		if !ok || active.Owner() != owner || !reflect.DeepEqual(active.Manifest, registration.Manifest) {
			return errors.New("provider owner changed during import")
		}
		provider, exists := current.Providers.Get(owner.ProviderID)
		if exists != configured || !reflect.DeepEqual(provider, before) {
			return errors.New("provider configuration changed during import")
		}
		return nil
	}
	// Copilot's integrated import has no identity lookup; normal interactive
	// login uses the same default account identity for this registration.
	if err := validate(); err != nil {
		return nil, false, err
	}
	accountBefore, err := captureCopilotImportAccounts(ctx, snapshot, registration.AccountNamespace)
	if err != nil {
		return nil, false, err
	}
	ctx = copilot.ContextWithImportEnvironment(ctx, snapshot.Getenv)
	ctx = providertransport.ContextWithOwnerValidator(ctx, validate)
	if err := validate(); err != nil {
		return nil, false, err
	}
	token, found, err := registration.OAuth.Import(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := validate(); err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	if token == nil || token.AccessToken == "" || token.RefreshToken == "" {
		return nil, false, errors.New("Copilot import returned incomplete credentials")
	}
	entry := accounts.FromToken("default", registration.ProviderID, token, nil)
	var accountWritten, configWritten bool
	err = func() error {
		release, err := lock.File(ctx, s.refreshLockPath(owner.ProviderID))
		if err != nil {
			return err
		}
		defer release()
		if err := s.lockAuthenticationWrite(ctx); err != nil {
			return err
		}
		defer s.writeMu.Unlock()
		if err := validate(); err != nil {
			return err
		}
		provider, err := s.providerConfigForCredentialLocked(s.Config(), owner.ProviderID)
		if err != nil {
			return err
		}
		applyOAuthTokenToProvider(&provider, token, registration)
		next := s.Config().cloneForWrite()
		next.Providers.Set(owner.ProviderID, provider)
		pending, err := accountBefore.BeginImport(ctx, owner.AccountNamespace, entry, validate)
		if err != nil {
			return err
		}
		defer pending.Close()
		selected, ok := pending.SelectedEntry()
		if !ok {
			return errors.New("imported account selection is unavailable")
		}
		if err := next.advanceRuntimeAuthenticationAccount(owner, &selected); err != nil {
			return err
		}
		fields := map[string]any{"api_key": token.AccessToken, "oauth": token, "owner": provider.Owner}
		if provider.Plugin != nil {
			fields["plugin"] = provider.Plugin
		}
		authored := make(map[string]any, len(fields))
		for key, value := range fields {
			authored[field+"."+key] = value
		}
		written, err := s.prepareAuthenticationCOW(ctx, next, path, authored, nil)
		if err != nil {
			return err
		}
		// Acquire every persistence lock and stage the exact file before the
		// first account write. Completion then inherits one absolute deadline.
		if err := lockAuthenticationMutex(ctx, s.mu.TryLock, s.mu.Unlock); err != nil {
			return err
		}
		defer s.mu.Unlock()
		releaseScopes, err := lockAuthenticationScopes(ctx, authenticationAdmission{globalPath: path, workspacePath: s.workspacePath})
		if err != nil {
			return err
		}
		defer releaseScopes()
		stagedData := slices.Clone(data)
		for _, key := range slices.Sorted(maps.Keys(fields)) {
			stagedData, err = sjson.SetBytes(stagedData, field+"."+key, fields[key])
			if err != nil {
				return err
			}
		}
		staged, err := stageAuthenticationScopeWrite(ctx, preimage, authenticationCredentialEdit{path: path, data: stagedData})
		if err != nil {
			return err
		}
		defer staged.Close()
		if err := validate(); err != nil {
			return err
		}
		committed, err := pending.Commit(ctx)
		accountWritten = committed.Written
		if err != nil {
			return err
		}
		completion, cancel := context.WithDeadline(context.WithoutCancel(ctx), committed.CompletionDeadline())
		defer cancel()
		if err := validate(); err != nil {
			return err
		}
		_, configWritten, err = staged.commit(completion, committed.CompletionDeadline())
		if err != nil {
			return err
		}
		if err := s.verifyAuthenticationCOW(completion, next, written); err != nil {
			return err
		}
		if _, err := pending.VerifyCommitted(completion); err != nil {
			return err
		}
		if err := validate(); err != nil {
			return err
		}
		if err := completion.Err(); err != nil {
			return err
		}
		s.captureStalenessSnapshot(append(slices.Clone(s.loadedPaths), path))
		s.setConfig(next)
		return nil
	}()
	if err != nil {
		if configWritten {
			return token, false, fmt.Errorf("imported account and provider config saved; runtime publication was not completed: %w", err)
		}
		if accountWritten {
			return token, false, fmt.Errorf("imported account saved; provider config was not updated: %w", err)
		}
		return nil, false, err
	}
	return token, true, nil
}

func captureCopilotImportAccounts(ctx context.Context, snapshot RuntimeSnapshot, namespace string) (accounts.Snapshot, error) {
	root := snapshot.Getenv("AI_CLI_DIR")
	if root == "" {
		home := snapshot.Getenv(authenticationHomeVariable())
		if !filepath.IsAbs(home) {
			return accounts.Snapshot{}, errors.New("captured account home is unavailable or not absolute")
		}
		root = filepath.Join(home, ".ai-cli")
	}
	if !filepath.IsAbs(root) {
		return accounts.Snapshot{}, errors.New("captured account directory is not absolute")
	}
	return accounts.CaptureStateAt(ctx, filepath.Join(root, "accounts.json"), []string{namespace})
}
