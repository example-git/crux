package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func ownerTestRegistration(providerID, pluginID string) providerregistry.Registration {
	return providerregistry.Registration{
		ProviderID:       providerID,
		AccountNamespace: pluginID + ".accounts",
		Construction:     providerregistry.ConstructionGenericJSON,
		OAuth:            &providerregistry.OAuthCapability{},
		Manifest: &manifest.Manifest{
			ID:      pluginID,
			Version: "1.0.0",
		},
	}
}

func waitForAuthSignalRegistration(t *testing.T, store *ConfigStore, owner providerregistry.RegistrationOwner) {
	t.Helper()
	require.Eventually(t, func() bool {
		store.authSignalMu.Lock()
		defer store.authSignalMu.Unlock()
		_, ok := store.authSignals[owner]
		return ok
	}, time.Second, time.Millisecond)
}

func forwardedAccountOwnerTestProvider(registration providerregistry.Registration, token *oauth.Token) ProviderConfig {
	return ProviderConfig{
		ID:         registration.ProviderID,
		APIKey:     token.AccessToken,
		OAuthToken: token,
		Owner:      providerOwnerReferenceForRegistration(registration),
		Plugin: &ProviderPluginReference{
			ID:      registration.Manifest.ID,
			Version: registration.Manifest.Version,
		},
	}
}

func newForwardedAccountOwnerTestStore(t *testing.T, registration providerregistry.Registration) *ConfigStore {
	t.Helper()
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	token := &oauth.Token{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		registration.ProviderID: forwardedAccountOwnerTestProvider(registration, token),
	})}
	root := t.TempDir()
	cfg.setDefaults(root, filepath.Join(root, "state"))
	cfg.bindProviderScan(ProviderScan{Registry: registry})
	store, _ := newProviderOwnerTestStore(t, cfg, registration)
	store.workingDir = root
	return store
}

func replaceForwardedAccountOwnerTestGeneration(t *testing.T, store *ConfigStore, registration providerregistry.Registration) {
	t.Helper()
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	token := &oauth.Token{AccessToken: "current-access", RefreshToken: "current-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	next := store.Config().cloneForWrite()
	next.Providers.Set(registration.ProviderID, forwardedAccountOwnerTestProvider(registration, token))
	next.bindProviderScan(ProviderScan{Registry: registry})
	store.writeMu.Lock()
	store.providerRegistry = registry
	store.setConfig(next)
	store.writeMu.Unlock()
}

func TestApplyEphemeralProviderStateRequiresExactForwardedAccountOwner(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	registration.AccountNamespace = "shared.accounts"
	exact := registration.Owner()
	missingNamespace := exact
	missingNamespace.AccountNamespace = ""
	wrongManifestID := exact
	wrongManifestID.ManifestID = "plugin.other"
	wrongManifestVersion := exact
	wrongManifestVersion.ManifestVersion = "2.0.0"
	wrongConstruction := exact
	wrongConstruction.Construction = providerregistry.ConstructionAnthropicMessages
	wrongAdapter := exact
	wrongAdapter.CompatibilityAdapter = providerregistry.ConstructionCodex

	for _, test := range []struct {
		name      string
		namespace string
		owner     providerregistry.RegistrationOwner
		errorText string
	}{
		{name: "empty namespace", owner: exact, errorText: "namespace is empty"},
		{name: "missing owner", namespace: exact.AccountNamespace, errorText: "missing its exact provider owner"},
		{name: "missing owner namespace", namespace: exact.AccountNamespace, owner: missingNamespace, errorText: "missing its exact provider owner"},
		{name: "conflicting namespace", namespace: "other.accounts", owner: exact, errorText: "declares conflicting namespace"},
		{name: "wrong manifest ID", namespace: exact.AccountNamespace, owner: wrongManifestID, errorText: "does not match the active exact owner"},
		{name: "wrong manifest version", namespace: exact.AccountNamespace, owner: wrongManifestVersion, errorText: "does not match the active exact owner"},
		{name: "wrong construction", namespace: exact.AccountNamespace, owner: wrongConstruction, errorText: "does not match the active exact owner"},
		{name: "wrong compatibility adapter", namespace: exact.AccountNamespace, owner: wrongAdapter, errorText: "does not match the active exact owner"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newForwardedAccountOwnerTestStore(t, registration)
			original := store.Config()
			err := store.ApplyEphemeralProviderState(nil, map[string]ForwardedAccount{
				test.namespace: {Owner: test.owner, Entry: accounts.Entry{ID: "forwarded", AccessToken: "secret"}},
			})
			require.ErrorContains(t, err, test.errorText)
			require.Same(t, original, store.Config())
			require.Empty(t, store.ephemeralAccounts)
			require.Empty(t, store.ephemeralProviders)
		})
	}
}

func TestApplyEphemeralProviderStateRequiresOAuthAccountOwner(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	registration.AccountNamespace = "shared.accounts"
	registration.OAuth = nil
	store := newForwardedAccountOwnerTestStore(t, registration)
	owner := registration.Owner()

	err := store.ApplyEphemeralProviderState(nil, map[string]ForwardedAccount{
		owner.AccountNamespace: {Owner: owner, Entry: accounts.Entry{ID: "forwarded", AccessToken: "secret"}},
	})
	require.ErrorContains(t, err, "does not support OAuth accounts")
	require.Empty(t, store.ephemeralAccounts)
}

func TestApplyEphemeralProviderStateCopiesExactForwardedAccount(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	registration.AccountNamespace = "shared.accounts"
	store := newForwardedAccountOwnerTestStore(t, registration)
	owner := registration.Owner()
	raw := []byte(`{"account_id":"forwarded"}`)

	require.NoError(t, store.ApplyEphemeralProviderState(nil, map[string]ForwardedAccount{
		owner.AccountNamespace: {
			Owner: owner,
			Entry: accounts.Entry{ID: "forwarded", DisplayName: "Forwarded", AccessToken: "secret", Raw: raw},
		},
	}))
	raw[0] = 'x'
	stored := store.ephemeralAccounts[owner.AccountNamespace]
	require.Equal(t, owner, stored.Owner)
	require.JSONEq(t, `{"account_id":"forwarded"}`, string(stored.Entry.Raw))
	entry, ok := store.EphemeralAccount(owner)
	require.True(t, ok)
	entry.Raw[0] = 'x'
	entry, ok = store.EphemeralAccount(owner)
	require.True(t, ok)
	require.JSONEq(t, `{"account_id":"forwarded"}`, string(entry.Raw))
}

func TestEphemeralAccountRejectsSameNamespaceOwnerReplacement(t *testing.T) {
	original := ownerTestRegistration("owner-test", "plugin.one")
	original.AccountNamespace = "shared.accounts"
	replacement := ownerTestRegistration("owner-test", "plugin.two")
	replacement.AccountNamespace = original.AccountNamespace
	store := newForwardedAccountOwnerTestStore(t, original)
	owner := original.Owner()
	require.NoError(t, store.ApplyEphemeralProviderState(nil, map[string]ForwardedAccount{
		owner.AccountNamespace: {Owner: owner, Entry: accounts.Entry{ID: "forwarded", AccessToken: "secret"}},
	}))
	_, ok := store.EphemeralAccount(owner)
	require.True(t, ok)

	replaceForwardedAccountOwnerTestGeneration(t, store, replacement)
	_, ok = store.EphemeralAccount(owner)
	require.False(t, ok)
	_, ok = store.EphemeralAccount(replacement.Owner())
	require.False(t, ok)
}

func TestApplyEphemeralTokenRejectsSameNamespaceOwnerReplacementWithoutMutation(t *testing.T) {
	original := ownerTestRegistration("owner-test", "plugin.one")
	original.AccountNamespace = "shared.accounts"
	replacement := ownerTestRegistration("owner-test", "plugin.two")
	replacement.AccountNamespace = original.AccountNamespace
	store := newForwardedAccountOwnerTestStore(t, original)
	owner := original.Owner()
	require.NoError(t, store.ApplyEphemeralProviderState(nil, map[string]ForwardedAccount{
		owner.AccountNamespace: {Owner: owner, Entry: accounts.Entry{ID: "forwarded", AccessToken: "old-account"}},
	}))
	replaceForwardedAccountOwnerTestGeneration(t, store, replacement)
	beforeConfig := store.Config()
	beforeForwarded := store.ephemeralAccounts[owner.AccountNamespace]

	err := store.applyEphemeralToken(&oauth.Token{AccessToken: "stale-access", RefreshToken: "stale-refresh"}, replacement.Owner())
	require.ErrorContains(t, err, "forwarded account owner")
	require.Same(t, beforeConfig, store.Config())
	require.Equal(t, beforeForwarded, store.ephemeralAccounts[owner.AccountNamespace])
}

func TestRemoveProviderCredentialsRejectsSameNamespaceOwnerReplacementWithoutMutation(t *testing.T) {
	original := ownerTestRegistration("owner-test", "plugin.one")
	original.AccountNamespace = "shared.accounts"
	replacement := ownerTestRegistration("owner-test", "plugin.two")
	replacement.AccountNamespace = original.AccountNamespace
	store := newForwardedAccountOwnerTestStore(t, original)
	owner := original.Owner()
	require.NoError(t, store.ApplyEphemeralProviderState(nil, map[string]ForwardedAccount{
		owner.AccountNamespace: {Owner: owner, Entry: accounts.Entry{ID: "forwarded", AccessToken: "old-account"}},
	}))
	replaceForwardedAccountOwnerTestGeneration(t, store, replacement)
	beforeConfig := store.Config()
	beforeForwarded := store.ephemeralAccounts[owner.AccountNamespace]

	err := store.RemoveProviderCredentials(ScopeGlobal, replacement.Owner())
	require.ErrorContains(t, err, "forwarded account owner")
	require.Same(t, beforeConfig, store.Config())
	require.Equal(t, beforeForwarded, store.ephemeralAccounts[owner.AccountNamespace])
}

func TestApplyEphemeralTokenPreservesMatchingForwardedAccountOwner(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	registration.AccountNamespace = "shared.accounts"
	store := newForwardedAccountOwnerTestStore(t, registration)
	owner := registration.Owner()
	require.NoError(t, store.ApplyEphemeralProviderState(nil, map[string]ForwardedAccount{
		owner.AccountNamespace: {
			Owner: owner,
			Entry: accounts.Entry{ID: "forwarded", DisplayName: "Forwarded", AccessToken: "old-account"},
		},
	}))
	refreshed := &oauth.Token{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}

	require.NoError(t, store.applyEphemeralToken(refreshed, owner))
	stored := store.ephemeralAccounts[owner.AccountNamespace]
	require.Equal(t, owner, stored.Owner)
	require.Equal(t, "forwarded", stored.Entry.ID)
	require.Equal(t, "Forwarded", stored.Entry.DisplayName)
	require.Equal(t, refreshed.AccessToken, stored.Entry.AccessToken)
	require.Equal(t, refreshed.RefreshToken, stored.Entry.RefreshToken)
	provider, ok := store.Config().Providers.Get(registration.ProviderID)
	require.True(t, ok)
	require.Equal(t, refreshed.AccessToken, provider.APIKey)
	require.Equal(t, refreshed, provider.OAuthToken)
}

func newProviderOwnerTestStore(t *testing.T, cfg *Config, registrations ...providerregistry.Registration) (*ConfigStore, string) {
	t.Helper()
	if cfg.Providers == nil {
		cfg.Providers = csync.NewMap[string, ProviderConfig]()
	}
	registry, err := providerregistry.New(registrations...)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "crux.json")
	require.NoError(t, os.WriteFile(path, mustMarshalConfig(cfg), 0o600))
	return &ConfigStore{
		config:           cfg,
		globalDataPath:   path,
		providerRegistry: registry,
		knownProviders: []catalog.Provider{{
			ID:          "owner-test",
			Name:        "Synthetic",
			APIEndpoint: "https://owner-test.example/v1",
			Type:        catalog.TypeOpenAICompat,
		}},
	}, path
}

func readOwnerMutationTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func ownerMutationTestProvider(registration providerregistry.Registration) ProviderConfig {
	return ProviderConfig{
		ID:    registration.ProviderID,
		Owner: providerOwnerReferenceForRegistration(registration),
		Plugin: &ProviderPluginReference{
			ID:      registration.Manifest.ID,
			Version: registration.Manifest.Version,
		},
		Models: []catalog.Model{{ID: "old-model"}, {ID: "new-model"}},
	}
}

func newOwnerMutationTestStore(t *testing.T, registration providerregistry.Registration) (*ConfigStore, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(root, "")
	cfg.Providers.Set(registration.ProviderID, ownerMutationTestProvider(registration))
	cfg.Models[SelectedModelTypeLarge] = SelectedModel{Provider: registration.ProviderID, Model: "old-model"}
	cfg.Models[SelectedModelTypeSmall] = SelectedModel{Provider: registration.ProviderID, Model: "old-model"}
	cfg.captureExplicitModels()
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	cfg.bindProviderScan(ProviderScan{Registry: registry})
	return newProviderOwnerTestStore(t, cfg, registration)
}

func replaceOwnerMutationTestGenerationLocked(t *testing.T, store *ConfigStore, registration providerregistry.Registration) *Config {
	t.Helper()
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	next := store.Config().cloneForWrite()
	next.Providers.Set(registration.ProviderID, ownerMutationTestProvider(registration))
	next.bindProviderScan(ProviderScan{Registry: registry})
	store.providerRegistry = registry
	store.setConfig(next)
	return next
}

func TestOwnerBoundPreferredModelMutationsRejectGenerationReplacementAtLockBoundary(t *testing.T) {
	for _, test := range []struct {
		name  string
		apply func(*ConfigStore, SelectedModel, providerregistry.RegistrationOwner) error
	}{
		{
			name: "persistent",
			apply: func(store *ConfigStore, model SelectedModel, owner providerregistry.RegistrationOwner) error {
				_, err := store.UpdatePreferredModelForOwner(ScopeGlobal, SelectedModelTypeLarge, model, owner)
				return err
			},
		},
		{
			name: "runtime override",
			apply: func(store *ConfigStore, model SelectedModel, owner providerregistry.RegistrationOwner) error {
				return store.OverridePreferredModelForOwner(SelectedModelTypeLarge, model, owner)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			initiating := ownerTestRegistration("owner-test", "plugin.one")
			replacement := ownerTestRegistration("owner-test", "plugin.two")
			store, path := newOwnerMutationTestStore(t, initiating)
			diskBefore, err := os.ReadFile(path)
			require.NoError(t, err)
			started := make(chan struct{})
			result := make(chan error, 1)

			store.writeMu.Lock()
			go func() {
				close(started)
				result <- test.apply(store, SelectedModel{Provider: "owner-test", Model: "new-model"}, initiating.Owner())
			}()
			<-started
			replacementConfig := replaceOwnerMutationTestGenerationLocked(t, store, replacement)
			store.writeMu.Unlock()

			require.ErrorContains(t, <-result, "active owner for provider owner-test changed")
			require.Same(t, replacementConfig, store.Config())
			require.Equal(t, SelectedModel{Provider: "owner-test", Model: "old-model"}, store.Config().Models[SelectedModelTypeLarge])
			require.Empty(t, store.Overrides().Models)
			diskAfter, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, diskBefore, diskAfter)
		})
	}
}

func TestSetProviderDisabledRejectsGenerationReplacementAtLockBoundary(t *testing.T) {
	initiating := ownerTestRegistration("owner-test", "plugin.one")
	replacement := ownerTestRegistration("owner-test", "plugin.two")
	store, path := newOwnerMutationTestStore(t, initiating)
	diskBefore, err := os.ReadFile(path)
	require.NoError(t, err)
	started := make(chan struct{})
	result := make(chan error, 1)

	store.writeMu.Lock()
	go func() {
		close(started)
		result <- store.SetProviderDisabled(ScopeGlobal, initiating.Owner(), true)
	}()
	<-started
	replacementConfig := replaceOwnerMutationTestGenerationLocked(t, store, replacement)
	store.writeMu.Unlock()

	require.ErrorContains(t, <-result, "registration owner for provider owner-test changed")
	require.Same(t, replacementConfig, store.Config())
	provider, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.False(t, provider.Disable)
	diskAfter, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, diskBefore, diskAfter)
}

func TestOwnerBoundMutationsPublishOnlyAfterPersistenceSucceeds(t *testing.T) {
	for _, test := range []struct {
		name  string
		apply func(*ConfigStore, providerregistry.RegistrationOwner) error
	}{
		{
			name: "preferred model",
			apply: func(store *ConfigStore, owner providerregistry.RegistrationOwner) error {
				_, err := store.UpdatePreferredModelForOwner(ScopeGlobal, SelectedModelTypeLarge, SelectedModel{
					Provider: "owner-test",
					Model:    "new-model",
				}, owner)
				return err
			},
		},
		{
			name: "provider disable",
			apply: func(store *ConfigStore, owner providerregistry.RegistrationOwner) error {
				return store.SetProviderDisabled(ScopeGlobal, owner, true)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			registration := ownerTestRegistration("owner-test", "plugin.one")
			store, path := newOwnerMutationTestStore(t, registration)
			configBefore := store.Config()
			overridesBefore := store.Overrides()
			diskBefore, err := os.ReadFile(path)
			require.NoError(t, err)
			store.writeFields = func(Scope, map[string]any) error {
				return errors.New("persistence blocked")
			}

			require.ErrorContains(t, test.apply(store, registration.Owner()), "persistence blocked")
			require.Same(t, configBefore, store.Config())
			require.Equal(t, overridesBefore, store.Overrides())
			diskAfter, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, diskBefore, diskAfter)
		})
	}
}

func TestOwnerBoundMutationsAcceptMatchingOwner(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	store, path := newOwnerMutationTestStore(t, registration)
	owner := registration.Owner()
	selected := SelectedModel{Provider: "owner-test", Model: "new-model"}

	state, err := store.UpdatePreferredModelForOwner(ScopeGlobal, SelectedModelTypeLarge, selected, owner)
	require.NoError(t, err)
	require.Equal(t, &OwnedSelectedModel{Model: selected, Owner: owner}, state.Large)
	require.Equal(t, selected, store.Config().Models[SelectedModelTypeLarge])
	require.Equal(t, selected, store.Overrides().Models[SelectedModelTypeLarge])
	require.Equal(t, "new-model", gjson.GetBytes(readOwnerMutationTestFile(t, path), "models.large.model").String())

	require.NoError(t, store.SetProviderDisabled(ScopeGlobal, owner, true))
	provider, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.True(t, provider.Disable)
	require.True(t, gjson.GetBytes(readOwnerMutationTestFile(t, path), "providers.owner-test.disable").Bool())

	require.NoError(t, store.SetProviderDisabled(ScopeGlobal, owner, false))
	provider, ok = store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.False(t, provider.Disable)
	require.False(t, gjson.GetBytes(readOwnerMutationTestFile(t, path), "providers.owner-test.disable").Bool())
}

func coreCopilotOwnerTestRegistration(t *testing.T) providerregistry.Registration {
	t.Helper()
	for _, registration := range providerregistry.Integrated() {
		if registration.ProviderID == string(catalog.ProviderCopilot) {
			return registration
		}
	}
	t.Fatal("core Copilot registration is unavailable")
	return providerregistry.Registration{}
}

func TestImportCopilotUsesCapturedOwnerCapability(t *testing.T) {
	registration := coreCopilotOwnerTestRegistration(t)
	imported := &oauth.Token{AccessToken: "imported-access", RefreshToken: "imported-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	calls := 0
	registration.OAuth.Import = func(context.Context) (*oauth.Token, bool, error) {
		calls++
		return imported, true, nil
	}
	providerID := string(catalog.ProviderCopilot)
	store, path := newProviderOwnerTestStore(t, &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {
			ID:    providerID,
			Owner: providerOwnerReferenceForRegistration(registration),
		},
	})}, registration)

	token, ok := store.ImportCopilot()
	require.True(t, ok)
	require.Same(t, imported, token)
	require.Equal(t, 1, calls)
	provider, ok := store.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, imported.AccessToken, provider.APIKey)
	require.Equal(t, imported, provider.OAuthToken)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, imported.AccessToken, gjson.GetBytes(data, "providers.copilot.api_key").String())
	require.Equal(t, imported.RefreshToken, gjson.GetBytes(data, "providers.copilot.oauth.refresh_token").String())
}

func TestImportCopilotRejectsGenerationReplacementAfterExternalRefresh(t *testing.T) {
	initiating := coreCopilotOwnerTestRegistration(t)
	replacement := initiating.Clone()
	replacement.OAuth.FlowID = "github-copilot-replacement"
	imported := &oauth.Token{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	current := &oauth.Token{AccessToken: "current-access", RefreshToken: "current-refresh", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}
	providerID := string(catalog.ProviderCopilot)
	var store *ConfigStore
	var path string
	var replacementConfig *Config
	initiating.OAuth.Import = func(context.Context) (*oauth.Token, bool, error) {
		registry, err := providerregistry.New(replacement)
		require.NoError(t, err)
		next := store.Config().cloneForWrite()
		provider, ok := next.Providers.Get(providerID)
		require.True(t, ok)
		provider.APIKey = current.AccessToken
		provider.OAuthToken = current
		provider.Owner = providerOwnerReferenceForRegistration(replacement)
		next.Providers.Set(providerID, provider)
		next.bindProviderScan(ProviderScan{Registry: registry})
		store.writeMu.Lock()
		store.providerRegistry = registry
		store.setConfig(next)
		replacementConfig = next
		store.writeMu.Unlock()
		return imported, true, nil
	}
	store, path = newProviderOwnerTestStore(t, &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {
			ID:    providerID,
			Owner: providerOwnerReferenceForRegistration(initiating),
		},
	})}, initiating)
	beforeDisk, err := os.ReadFile(path)
	require.NoError(t, err)

	token, ok := store.ImportCopilot()
	require.False(t, ok)
	require.Nil(t, token)
	require.Same(t, replacementConfig, store.Config())
	provider, ok := store.Config().Providers.Get(providerID)
	require.True(t, ok)
	require.Equal(t, current.AccessToken, provider.APIKey)
	require.Equal(t, current, provider.OAuthToken)
	afterDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
	_, err = os.Stat(filepath.Join(filepath.Dir(path), "locks"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSetProviderOAuthTokenPersistsExactPluginOwner(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	store, path := newProviderOwnerTestStore(t, &Config{}, registration)
	oldConfig := store.Config()
	token := &oauth.Token{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}

	require.NoError(t, store.SetProviderOAuthToken(ScopeGlobal, registration.Owner(), token))

	provider, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.Equal(t, "new-access", provider.APIKey)
	require.Equal(t, token, provider.OAuthToken)
	require.Equal(t, &ProviderOwnerReference{Type: ProviderOwnerPlugin, Construction: providerregistry.ConstructionGenericJSON}, provider.Owner)
	require.Equal(t, &ProviderPluginReference{ID: "plugin.one", Version: "1.0.0"}, provider.Plugin)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "new-access", gjson.GetBytes(data, "providers.owner-test.api_key").String())
	require.Equal(t, "new-refresh", gjson.GetBytes(data, "providers.owner-test.oauth.refresh_token").String())
	require.Equal(t, "plugin", gjson.GetBytes(data, "providers.owner-test.owner.type").String())
	require.Equal(t, "generic-json", gjson.GetBytes(data, "providers.owner-test.owner.construction").String())
	require.Equal(t, "plugin.one", gjson.GetBytes(data, "providers.owner-test.plugin.id").String())
	require.Equal(t, "1.0.0", gjson.GetBytes(data, "providers.owner-test.plugin.version").String())
	_, oldConfigured := oldConfig.Providers.Get("owner-test")
	require.False(t, oldConfigured)
}

func TestSetProviderOAuthTokenRejectsOwnerChangeWithoutMutation(t *testing.T) {
	initiating := ownerTestRegistration("owner-test", "plugin.one")
	current := ownerTestRegistration("owner-test", "plugin.two")
	oldToken := &oauth.Token{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	beforeProvider := ProviderConfig{
		ID:         "owner-test",
		APIKey:     oldToken.AccessToken,
		OAuthToken: oldToken,
		Plugin:     &ProviderPluginReference{ID: "plugin.two", Version: "1.0.0"},
	}
	store, path := newProviderOwnerTestStore(t, &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"owner-test": beforeProvider})}, current)
	beforeDisk, err := os.ReadFile(path)
	require.NoError(t, err)

	err = store.SetProviderOAuthToken(ScopeGlobal, initiating.Owner(), &oauth.Token{AccessToken: "new-access", RefreshToken: "new-refresh"})
	require.ErrorContains(t, err, "changed before the token could be persisted")

	afterProvider, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.Equal(t, beforeProvider, afterProvider)
	afterDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
}

func TestRemoveProviderCredentialsRequiresExactOwner(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	token := &oauth.Token{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	provider := ProviderConfig{
		ID:             "owner-test",
		APIKey:         token.AccessToken,
		APIKeyTemplate: "$SYNTHETIC_TOKEN",
		OAuthToken:     token,
		Plugin:         &ProviderPluginReference{ID: "plugin.one", Version: "1.0.0"},
	}
	store, path := newProviderOwnerTestStore(t, &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"owner-test": provider})}, registration)

	require.NoError(t, store.RemoveProviderCredentials(ScopeGlobal, registration.Owner()))

	updated, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.Empty(t, updated.APIKey)
	require.Empty(t, updated.APIKeyTemplate)
	require.Nil(t, updated.OAuthToken)
	require.Equal(t, provider.Plugin, updated.Plugin)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(data, "providers.owner-test.api_key").Exists())
	require.False(t, gjson.GetBytes(data, "providers.owner-test.oauth").Exists())
	require.Equal(t, "plugin.one", gjson.GetBytes(data, "providers.owner-test.plugin.id").String())
}

func TestRemoveProviderCredentialsRejectsOwnerChangeWithoutMutation(t *testing.T) {
	initiating := ownerTestRegistration("owner-test", "plugin.one")
	current := ownerTestRegistration("owner-test", "plugin.two")
	token := &oauth.Token{AccessToken: "old-access", RefreshToken: "old-refresh"}
	beforeProvider := ProviderConfig{
		ID:         "owner-test",
		APIKey:     token.AccessToken,
		OAuthToken: token,
		Plugin:     &ProviderPluginReference{ID: "plugin.two", Version: "1.0.0"},
	}
	store, path := newProviderOwnerTestStore(t, &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"owner-test": beforeProvider})}, current)
	beforeDisk, err := os.ReadFile(path)
	require.NoError(t, err)

	err = store.RemoveProviderCredentials(ScopeGlobal, initiating.Owner())
	require.ErrorContains(t, err, "changed before credentials could be removed")

	afterProvider, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.Equal(t, beforeProvider, afterProvider)
	afterDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
}

func TestSetProviderAPIKeyRejectsBareOAuthToken(t *testing.T) {
	store, path := newProviderOwnerTestStore(t, &Config{})
	beforeDisk, err := os.ReadFile(path)
	require.NoError(t, err)

	err = store.SetProviderAPIKey(ScopeGlobal, "owner-test", &oauth.Token{AccessToken: "unowned"})
	require.ErrorContains(t, err, "missing its initiating owner")
	require.Equal(t, 0, store.Config().Providers.Len())
	afterDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
}

func TestProviderCredentialOwner(t *testing.T) {
	owner := ownerTestRegistration("owner-test", "plugin.one").Owner()
	token := &oauth.Token{AccessToken: "token"}

	tests := []struct {
		name       string
		providerID string
		credential any
		want       providerregistry.RegistrationOwner
		wantErr    string
	}{
		{name: "api key", providerID: owner.ProviderID, credential: ProviderAPIKeyCredential{Owner: owner, APIKey: "key"}, want: owner},
		{name: "oauth", providerID: owner.ProviderID, credential: ProviderOAuthCredential{Owner: owner, Token: token}, want: owner},
		{name: "api key owner mismatch", providerID: "other", credential: ProviderAPIKeyCredential{Owner: owner, APIKey: "key"}, wantErr: "does not match credential provider"},
		{name: "oauth owner mismatch", providerID: "other", credential: ProviderOAuthCredential{Owner: owner, Token: token}, wantErr: "does not match credential provider"},
		{name: "nil oauth", providerID: owner.ProviderID, credential: ProviderOAuthCredential{Owner: owner}, wantErr: "OAuth token is nil"},
		{name: "bare api key", providerID: owner.ProviderID, credential: "key", wantErr: "missing its initiating owner"},
		{name: "bare oauth", providerID: owner.ProviderID, credential: token, wantErr: "missing its initiating owner"},
		{name: "unsupported", providerID: owner.ProviderID, credential: nil, wantErr: "unsupported API key type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ProviderCredentialOwner(tt.providerID, tt.credential)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.Zero(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestAuthSignalsAreExactOwnerBound(t *testing.T) {
	ownerA := ownerTestRegistration("owner-test", "plugin.one").Owner()
	ownerB := ownerA
	ownerB.ManifestVersion = "2.0.0"

	for _, tt := range []struct {
		name   string
		first  providerregistry.RegistrationOwner
		second providerregistry.RegistrationOwner
	}{
		{name: "owner a first", first: ownerA, second: ownerB},
		{name: "owner b first", first: ownerB, second: ownerA},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &ConfigStore{}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			firstResult := make(chan error, 1)
			secondResult := make(chan error, 1)
			go func() { firstResult <- store.WaitForTokenChange(ctx, tt.first) }()
			go func() { secondResult <- store.WaitForTokenChange(ctx, tt.second) }()
			waitForAuthSignalRegistration(t, store, tt.first)
			waitForAuthSignalRegistration(t, store, tt.second)

			store.SignalAuthComplete(tt.first)
			require.NoError(t, <-firstResult)
			select {
			case err := <-secondResult:
				t.Fatalf("signal for owner %+v woke owner %+v: %v", tt.first, tt.second, err)
			default:
			}

			store.SignalAuthComplete(tt.second)
			require.NoError(t, <-secondResult)
		})
	}
}

func TestAuthPreSignalIsExactOwnerBound(t *testing.T) {
	ownerA := ownerTestRegistration("owner-test", "plugin.one").Owner()
	ownerB := ownerA
	ownerB.ManifestVersion = "2.0.0"
	store := &ConfigStore{}
	store.SignalAuthComplete(ownerA)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, store.WaitForTokenChange(ctx, ownerA))

	result := make(chan error, 1)
	go func() { result <- store.WaitForTokenChange(ctx, ownerB) }()
	waitForAuthSignalRegistration(t, store, ownerB)
	select {
	case err := <-result:
		t.Fatalf("pre-signal for owner %+v woke owner %+v: %v", ownerA, ownerB, err)
	default:
	}
	store.SignalAuthComplete(ownerB)
	require.NoError(t, <-result)
}

func TestSetProviderAPIKeyPersistsExactPresetOwner(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	providerPresetReferences = map[string]ProviderPresetReference{
		"owner-test": {ID: "preset.one", Version: "1.2.3", Digest: "digest-one"},
	}
	store, path := newProviderOwnerTestStore(t, &Config{})

	owner, ok := store.RuntimeSnapshot().ProviderOwner("owner-test")
	require.True(t, ok)
	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "owner-test", ProviderAPIKeyCredential{Owner: owner, APIKey: "new-key"}))

	provider, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.Equal(t, "new-key", provider.APIKey)
	require.Equal(t, &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat}, provider.Owner)
	require.Equal(t, &ProviderPresetReference{ID: "preset.one", Version: "1.2.3", Digest: "digest-one"}, provider.Preset)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "preset", gjson.GetBytes(data, "providers.owner-test.owner.type").String())
	require.Equal(t, "openai-compat", gjson.GetBytes(data, "providers.owner-test.owner.construction").String())
	require.Equal(t, "preset.one", gjson.GetBytes(data, "providers.owner-test.preset.id").String())
	require.Equal(t, "1.2.3", gjson.GetBytes(data, "providers.owner-test.preset.version").String())
	require.Equal(t, "digest-one", gjson.GetBytes(data, "providers.owner-test.preset.digest").String())
}

func TestValidateActiveProviderOwnerRequiresMatchingEnabledOwner(t *testing.T) {
	t.Run("matching", func(t *testing.T) {
		registration := ownerTestRegistration("owner-test", "plugin.one")
		store := newForwardedAccountOwnerTestStore(t, registration)
		require.NoError(t, store.ValidateActiveProviderOwner(registration.Owner()))
	})

	t.Run("disabled", func(t *testing.T) {
		registration := ownerTestRegistration("owner-test", "plugin.one")
		store := newForwardedAccountOwnerTestStore(t, registration)
		store.writeMu.Lock()
		next := store.Config().cloneForWrite()
		provider, ok := next.Providers.Get(registration.ProviderID)
		require.True(t, ok)
		provider.Disable = true
		next.Providers.Set(registration.ProviderID, provider)
		store.setConfig(next)
		store.writeMu.Unlock()
		require.ErrorContains(t, store.ValidateActiveProviderOwner(registration.Owner()), "changed")
	})

	t.Run("replaced", func(t *testing.T) {
		initial := ownerTestRegistration("owner-test", "plugin.one")
		store := newForwardedAccountOwnerTestStore(t, initial)
		replacement := ownerTestRegistration("owner-test", "plugin.two")
		replaceForwardedAccountOwnerTestGeneration(t, store, replacement)
		require.ErrorContains(t, store.ValidateActiveProviderOwner(initial.Owner()), "changed")
		require.NoError(t, store.ValidateActiveProviderOwner(replacement.Owner()))
	})
}

func TestRuntimeSnapshotRequiresFullPassedOwnerTuple(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	registration.CompatibilityAdapter = providerregistry.ConstructionOpenAIResponses
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	exact := ProviderConfig{
		ID:     "owner-test",
		Owner:  providerOwnerReferenceForRegistration(registration),
		Plugin: &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version},
	}
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"owner-test": exact})}
	snapshot := RuntimeSnapshot{config: cfg, registry: registry}

	resolved, ok := snapshot.ProviderRegistrationFor("owner-test", exact)
	require.True(t, ok)
	require.Equal(t, registration.Owner(), resolved.Owner())
	owner, ok := snapshot.ProviderOwnerFor("owner-test", exact)
	require.True(t, ok)
	require.Equal(t, registration.Owner(), owner)

	missingOwner := exact
	missingOwner.Owner = nil
	wrongType := exact
	wrongType.Owner = &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}
	wrongConstruction := exact
	wrongConstruction.Owner = &ProviderOwnerReference{Type: ProviderOwnerPlugin, Construction: providerregistry.ConstructionAnthropicMessages, CompatibilityAdapter: registration.CompatibilityAdapter}
	wrongAdapter := exact
	wrongAdapter.Owner = &ProviderOwnerReference{Type: ProviderOwnerPlugin, Construction: registration.Construction, CompatibilityAdapter: providerregistry.ConstructionGeminiContent}
	wrongVersion := exact
	wrongVersion.Plugin = &ProviderPluginReference{ID: registration.Manifest.ID, Version: "2.0.0"}
	wrongProviderID := exact
	wrongProviderID.ID = "other-provider"
	for name, provider := range map[string]ProviderConfig{
		"missing owner":      missingOwner,
		"wrong owner type":   wrongType,
		"wrong construction": wrongConstruction,
		"wrong adapter":      wrongAdapter,
		"wrong version":      wrongVersion,
		"wrong provider ID":  wrongProviderID,
	} {
		t.Run(name, func(t *testing.T) {
			_, registered := snapshot.ProviderRegistrationFor("owner-test", provider)
			require.False(t, registered)
			_, owned := snapshot.ProviderOwnerFor("owner-test", provider)
			require.False(t, owned)
		})
	}
}

func TestRuntimeSnapshotPreservesExplicitCustomOwnerAgainstSameIDPlugin(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	provider := ProviderConfig{
		ID:    "owner-test",
		Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
	}
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"owner-test": provider})}
	snapshot := RuntimeSnapshot{config: cfg, registry: registry}

	_, registered := snapshot.ProviderRegistrationFor("owner-test", provider)
	require.False(t, registered)
	expected := providerregistry.RegistrationOwner{ProviderID: "owner-test"}
	owner, ok := snapshot.ProviderOwnerFor("owner-test", provider)
	require.True(t, ok)
	require.Equal(t, expected, owner)
	owner, ok = snapshot.ProviderOwner("owner-test")
	require.True(t, ok)
	require.Equal(t, expected, owner)
}

func TestRuntimeSnapshotProviderForConstructionAuthorizesExactOwnerTypes(t *testing.T) {
	plugin := ownerTestRegistration("plugin-owned", "plugin.one")
	registrations := providerregistry.Integrated()
	registrations = append(registrations, plugin)
	registry, err := providerregistry.New(registrations...)
	require.NoError(t, err)
	preset := ProviderPresetReference{ID: "preset.one", Version: "1.0.0", Digest: "digest-one"}
	providers := map[string]ProviderConfig{
		string(catalog.ProviderCopilot): {
			Owner: &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: providerregistry.ConstructionCopilot},
		},
		"plugin-owned": {
			Owner:  providerOwnerReferenceForRegistration(plugin),
			Plugin: &ProviderPluginReference{ID: plugin.Manifest.ID, Version: plugin.Manifest.Version},
		},
		"preset-owned": {
			Owner:  &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat},
			Preset: &preset,
		},
		"custom-owned": {
			Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
		},
	}
	cfg := &Config{Providers: csync.NewMapFrom(providers)}
	cfg.bindProviderScan(ProviderScan{
		Registry: registry,
		presetReferences: map[string]ProviderPresetReference{
			"preset-owned": preset,
		},
	})
	snapshot := RuntimeSnapshot{config: cfg, registry: registry}

	for _, test := range []struct {
		providerID string
		registered bool
	}{
		{providerID: string(catalog.ProviderCopilot), registered: true},
		{providerID: "plugin-owned", registered: true},
		{providerID: "preset-owned"},
		{providerID: "custom-owned"},
	} {
		t.Run(test.providerID, func(t *testing.T) {
			provider := providers[test.providerID]
			resolved, registration, registered, err := snapshot.ProviderForConstruction(test.providerID, provider)
			require.NoError(t, err)
			require.Equal(t, test.providerID, resolved.ID)
			require.Equal(t, test.registered, registered)
			if registered {
				require.Equal(t, test.providerID, registration.ProviderID)
			} else {
				require.Empty(t, registration.ProviderID)
			}
		})
	}
}

func TestRuntimeSnapshotDoesNotReclassifyUnavailablePresetAsCustom(t *testing.T) {
	registry, err := providerregistry.New()
	require.NoError(t, err)
	configured := ProviderPresetReference{ID: "preset.expected", Version: "1.0.0"}
	for _, test := range []struct {
		name   string
		active map[string]ProviderPresetReference
	}{
		{name: "missing"},
		{name: "mismatched", active: map[string]ProviderPresetReference{
			"owner-test": {ID: "preset.other", Version: "1.0.0", Digest: "other-digest"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"owner-test": {ID: "owner-test", Preset: &configured},
			})}
			cfg.bindProviderScan(ProviderScan{Registry: registry, presetReferences: test.active})
			store, _ := newProviderOwnerTestStore(t, cfg)

			_, ok := store.RuntimeSnapshot().ProviderOwner("owner-test")
			require.False(t, ok)
			require.ErrorContains(t, store.ValidateRegistrationOwner(providerregistry.RegistrationOwner{ProviderID: "owner-test"}), "changed")
		})
	}
}

func TestRefreshOAuthTokenForOwnerRejectsGenerationReplacementDuringExchange(t *testing.T) {
	initiating := ownerTestRegistration("owner-test", "plugin.one")
	initiating.OAuth.Refresh = func(context.Context, string) (*oauth.Token, error) {
		return nil, nil
	}
	current := ownerTestRegistration("owner-test", "plugin.two")
	current.OAuth.Refresh = initiating.OAuth.Refresh
	oldToken := &oauth.Token{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	store, _ := newProviderOwnerTestStore(t, &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"owner-test": {
			ID:         "owner-test",
			APIKey:     oldToken.AccessToken,
			OAuthToken: oldToken,
			Plugin:     &ProviderPluginReference{ID: "plugin.one", Version: "1.0.0"},
		},
	})}, initiating)
	store.ephemeralProviders = map[string]struct{}{"owner-test": {}}
	store.ephemeralProviderConfigs = map[string]ProviderConfig{}
	store.exchangeToken = func(context.Context, string, string) (*oauth.Token, error) {
		registry, err := providerregistry.New(current)
		require.NoError(t, err)
		store.writeMu.Lock()
		next := store.Config().cloneForWrite()
		provider, ok := next.Providers.Get("owner-test")
		require.True(t, ok)
		provider.APIKey = "new-generation-access"
		provider.OAuthToken = &oauth.Token{AccessToken: "new-generation-access", RefreshToken: "new-generation-refresh"}
		provider.Plugin = &ProviderPluginReference{ID: "plugin.two", Version: "1.0.0"}
		next.Providers.Set("owner-test", provider)
		store.providerRegistry = registry
		store.setConfig(next)
		store.writeMu.Unlock()
		return &oauth.Token{AccessToken: "stale-refresh-access", RefreshToken: "stale-refresh-token"}, nil
	}

	_, err := store.RefreshOAuthTokenForOwner(t.Context(), ScopeGlobal, initiating.Owner())
	require.ErrorContains(t, err, "changed")
	provider, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.Equal(t, "new-generation-access", provider.APIKey)
	require.Equal(t, "new-generation-refresh", provider.OAuthToken.RefreshToken)
	require.Equal(t, "plugin.two", provider.Plugin.ID)
}

func TestSetProviderAPIKeyRejectsSameIDOwnerReplacementWithoutMutation(t *testing.T) {
	initiating := ownerTestRegistration("owner-test", "plugin.one")
	current := ownerTestRegistration("owner-test", "plugin.two")
	provider := ProviderConfig{
		ID:     "owner-test",
		APIKey: "current-key",
		Plugin: &ProviderPluginReference{ID: "plugin.two", Version: "1.0.0"},
	}
	store, path := newProviderOwnerTestStore(t, &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"owner-test": provider})}, current)
	beforeDisk, err := os.ReadFile(path)
	require.NoError(t, err)

	err = store.SetProviderAPIKey(ScopeGlobal, "owner-test", ProviderAPIKeyCredential{Owner: initiating.Owner(), APIKey: "stale-key"})
	require.ErrorContains(t, err, "changed")
	actual, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.Equal(t, provider, actual)
	afterDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
}

func TestSetResolvedProviderAPIKeyRejectsOwnerAndTemplateReplacement(t *testing.T) {
	initiating := ownerTestRegistration("owner-test", "plugin.one")
	current := ownerTestRegistration("owner-test", "plugin.two")
	provider := ProviderConfig{
		ID:             "owner-test",
		APIKey:         "new-generation-key",
		APIKeyTemplate: "$NEW_GENERATION_KEY",
		Plugin:         &ProviderPluginReference{ID: "plugin.two", Version: "1.0.0"},
	}
	store, _ := newProviderOwnerTestStore(t, &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"owner-test": provider})}, current)

	err := store.SetResolvedProviderAPIKey(initiating.Owner(), "$OLD_GENERATION_KEY", "stale-key")
	require.ErrorContains(t, err, "changed")
	actual, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.Equal(t, provider, actual)

	err = store.SetResolvedProviderAPIKey(current.Owner(), "$OLD_GENERATION_KEY", "stale-key")
	require.ErrorContains(t, err, "template")
	actual, ok = store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.Equal(t, provider, actual)
}

func TestSetProviderAPIKeyRejectsMismatchedPresetWithoutMutation(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	providerPresetReferences = map[string]ProviderPresetReference{
		"owner-test": {ID: "preset.active", Version: "2.0.0"},
	}
	beforeProvider := ProviderConfig{ID: "owner-test", APIKey: "old-key", Preset: &ProviderPresetReference{ID: "preset.expected", Version: "1.0.0"}}
	store, path := newProviderOwnerTestStore(t, &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"owner-test": beforeProvider})})
	beforeDisk, err := os.ReadFile(path)
	require.NoError(t, err)

	owner := providerregistry.RegistrationOwner{ProviderID: "owner-test", HasPreset: true, PresetID: "preset.expected", PresetVersion: "1.0.0"}
	err = store.SetProviderAPIKey(ScopeGlobal, "owner-test", ProviderAPIKeyCredential{Owner: owner, APIKey: "new-key"})
	require.ErrorContains(t, err, "changed")
	afterProvider, ok := store.Config().Providers.Get("owner-test")
	require.True(t, ok)
	require.Equal(t, beforeProvider, afterProvider)
	afterDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
}
