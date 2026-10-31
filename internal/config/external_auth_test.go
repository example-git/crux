package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func externalAuthTestStore(t *testing.T, policy *manifest.ExternalAuthPolicy, token *oauth.Token, configure ...func(*providerregistry.Registration)) (*ConfigStore, providerregistry.Registration) {
	t.Helper()
	t.Setenv("AI_CLI_DIR", t.TempDir())
	registration := ownerTestRegistration("external-provider", "external.plugin")
	registration.Manifest.Capabilities.Credentials = []manifest.Credential{{ID: "account", Kind: "oauth2"}}
	registration.Manifest.Capabilities.ExternalAuth = policy
	registration.OAuth.Refresh = func(context.Context, string) (*oauth.Token, error) { return selectedRefreshToken(), nil }
	for _, change := range configure {
		change(&registration)
	}
	provider := forwardedAccountOwnerTestProvider(registration, token)
	store := NewTestStoreWithRegistrations(&Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{registration.ProviderID: provider}),
		Models:    map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: registration.ProviderID, Model: "model"}},
	}, registration)
	store.config.providerScan.pluginStatuses[registration.Manifest.ID] = providerplugin.Status{ID: registration.Manifest.ID, ProviderID: registration.ProviderID, Version: registration.Manifest.Version, Digest: "synthetic-bundle", State: providerplugin.StateRegistered}
	store.globalDataPath = filepath.Join(t.TempDir(), "crux.json")
	store.workingDir = filepath.Join(filepath.Dir(store.globalDataPath), "project")
	require.NoError(t, os.MkdirAll(store.workingDir, 0o700))
	store.baseEnvironment = env.NewFromMap(map[string]string{
		"HOME": filepath.Dir(store.globalDataPath), "USERPROFILE": filepath.Dir(store.globalDataPath),
		"AI_CLI_DIR": os.Getenv("AI_CLI_DIR"), "CRUX_GLOBAL_DATA": filepath.Dir(store.globalDataPath),
		"CRUX_GLOBAL_CONFIG": filepath.Join(filepath.Dir(store.globalDataPath), "config"),
	})
	store.effectiveEnvironment = cloneEnvironment(store.baseEnvironment)
	store.resolver = IdentityResolver()
	data, err := json.Marshal(store.Config())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.globalDataPath, data, 0o600))
	if token.AccessToken != "" && registration.AccountNamespace != "" {
		require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, accounts.FromToken("selected", "Selected", token, nil)))
	}
	return store, registration
}

func externalAuthTestPolicy() *manifest.ExternalAuthPolicy {
	return &manifest.ExternalAuthPolicy{Service: "fixture", Credential: "account", Method: "oauth", TokenExport: "access-token"}
}

func TestExternalAuthenticationPolicyAndWithholding(t *testing.T) {
	for _, test := range []struct {
		name    string
		policy  *manifest.ExternalAuthPolicy
		include bool
		want    ExternalAuthentication
	}{
		{name: "no declaration"},
		{name: "different service", policy: &manifest.ExternalAuthPolicy{Service: "other", Credential: "account", Method: "oauth", TokenExport: "access-token"}, include: true},
		{name: "withheld by default", policy: externalAuthTestPolicy(), want: ExternalAuthentication{Required: true, Method: "oauth"}},
		{name: "included", policy: externalAuthTestPolicy(), include: true, want: ExternalAuthentication{Required: true, Method: "oauth", Token: "synthetic-fresh"}},
		{name: "policy forbids export", policy: &manifest.ExternalAuthPolicy{Service: "fixture", Credential: "account", Method: "oauth", TokenExport: "never"}, include: true, want: ExternalAuthentication{Required: true, Method: "oauth"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _ := externalAuthTestStore(t, test.policy, selectedRefreshToken())
			got, err := store.ExternalAuthentication(t.Context(), ExternalAuthenticationRequest{Service: "fixture", IncludeToken: test.include})
			require.NoError(t, err)
			require.Equal(t, test.want, got)
			require.NotContains(t, fmt.Sprintf("%+v %#v", got, got), "synthetic-fresh")
		})
	}
}

func TestExternalAuthenticationRejectsUnavailableState(t *testing.T) {
	for _, action := range []string{"remote", "disabled", "owner-mismatch", "account-mismatch", "missing-account", "cancelled", "invalid-service"} {
		t.Run(action, func(t *testing.T) {
			store, registration := externalAuthTestStore(t, externalAuthTestPolicy(), selectedRefreshToken())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := ExternalAuthenticationRequest{Service: "fixture", IncludeToken: true}
			switch action {
			case "remote":
				store.clientRuntime = &clientRuntimeState{}
			case "disabled":
				store.mutateInMemory(func(cfg *Config) {
					provider, _ := cfg.Providers.Get(registration.ProviderID)
					provider.Disable = true
					cfg.Providers.Set(registration.ProviderID, provider)
				})
			case "owner-mismatch":
				store.mutateInMemory(func(cfg *Config) {
					provider, _ := cfg.Providers.Get(registration.ProviderID)
					provider.Plugin = &ProviderPluginReference{ID: "different.plugin", Version: "1.0.0"}
					cfg.Providers.Set(registration.ProviderID, provider)
				})
			case "account-mismatch":
				require.NoError(t, accounts.Save(ctx, registration.AccountNamespace, accounts.Entry{ID: "other", AccessToken: "other-secret"}))
			case "missing-account":
				require.NoError(t, accounts.RemoveProvider(ctx, registration.AccountNamespace))
			case "cancelled":
				cancel()
			case "invalid-service":
				request.Service = "invalid service"
			}
			got, err := store.ExternalAuthentication(ctx, request)
			require.Error(t, err)
			require.Empty(t, got.Token)
			require.NotContains(t, err.Error(), "synthetic-fresh")
		})
	}
}

func TestExternalAuthenticationLoggedOutAndExpired(t *testing.T) {
	for _, token := range []*oauth.Token{{}, {AccessToken: "expired", ExpiresAt: time.Now().Add(-time.Hour).Unix()}} {
		store, registration := externalAuthTestStore(t, externalAuthTestPolicy(), token)
		if token.AccessToken == "" {
			store.mutateInMemory(func(cfg *Config) {
				provider, _ := cfg.Providers.Get(registration.ProviderID)
				provider.OAuthToken = nil
				cfg.Providers.Set(registration.ProviderID, provider)
			})
		}
		got, err := store.ExternalAuthentication(t.Context(), ExternalAuthenticationRequest{Service: "fixture", IncludeToken: true, RefreshToken: true})
		require.NoError(t, err)
		require.Equal(t, ExternalAuthentication{Required: true}, got)
	}
}

func TestExternalAuthenticationRefreshPersistsBeforeExport(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprint(force), func(t *testing.T) {
			token := selectedRefreshToken()
			token.AccessToken, token.RefreshToken = "synthetic-before", "synthetic-before-refresh"
			if !force {
				token.ExpiresAt = time.Now().Add(-time.Hour).Unix()
			}
			store, registration := externalAuthTestStore(t, externalAuthTestPolicy(), token)
			calls := 0
			store.exchangeToken = func(_ context.Context, providerID, refreshToken string) (*oauth.Token, error) {
				calls++
				require.Equal(t, registration.ProviderID, providerID)
				require.Equal(t, token.RefreshToken, refreshToken)
				return selectedRefreshToken(), nil
			}
			got, err := store.ExternalAuthentication(t.Context(), ExternalAuthenticationRequest{Service: "fixture", IncludeToken: true, RefreshToken: force})
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, "synthetic-fresh", got.Token)
			entry, err := accounts.Active(t.Context(), registration.AccountNamespace)
			require.NoError(t, err)
			require.Equal(t, got.Token, entry.AccessToken)
			data, err := os.ReadFile(store.globalDataPath)
			require.NoError(t, err)
			var saved Config
			require.NoError(t, json.Unmarshal(data, &saved))
			provider, ok := saved.Providers.Get(registration.ProviderID)
			require.True(t, ok)
			require.Equal(t, got.Token, provider.APIKey)
			require.Equal(t, entry.RefreshToken, provider.OAuthToken.RefreshToken)
		})
	}
}

func TestExternalAuthenticationRefreshFailureAndSelectionChange(t *testing.T) {
	for _, change := range []bool{false, true} {
		store, _ := externalAuthTestStore(t, externalAuthTestPolicy(), selectedRefreshToken())
		store.exchangeToken = func(context.Context, string, string) (*oauth.Token, error) {
			if !change {
				return nil, errors.New("synthetic-secret-in-upstream-error")
			}
			store.mutateInMemory(func(cfg *Config) {
				selected := cfg.Models[SelectedModelTypeLarge]
				selected.Model = "changed"
				cfg.Models[SelectedModelTypeLarge] = selected
			})
			return selectedRefreshToken(), nil
		}
		got, err := store.ExternalAuthentication(t.Context(), ExternalAuthenticationRequest{Service: "fixture", IncludeToken: true, RefreshToken: true})
		require.Error(t, err)
		require.Empty(t, got.Token)
		require.NotContains(t, err.Error(), "synthetic-secret")
	}
}

func TestExternalAuthenticationLoadedCandidate(t *testing.T) {
	store, registration := externalAuthTestStore(t, externalAuthTestPolicy(), &oauth.Token{})
	store.mutateInMemory(func(cfg *Config) {
		provider, _ := cfg.Providers.Get(registration.ProviderID)
		provider.OAuthToken = nil
		cfg.authenticationCandidates = map[string]ProviderConfig{registration.ProviderID: provider}
		cfg.Providers.Del(registration.ProviderID)
	})
	got, err := store.ExternalAuthentication(t.Context(), ExternalAuthenticationRequest{Service: "fixture", IncludeToken: true})
	require.NoError(t, err)
	require.Equal(t, ExternalAuthentication{Required: true}, got)
}

func TestExternalAuthenticationScalarCredentials(t *testing.T) {
	for _, kind := range []string{"api-key", "bearer"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := externalAuthTestStore(t, externalAuthTestPolicy(), selectedRefreshToken(), func(registration *providerregistry.Registration) {
				registration.Manifest.Capabilities.Credentials[0].Kind = kind
				registration.AccountNamespace = ""
			})
			got, err := store.ExternalAuthentication(t.Context(), ExternalAuthenticationRequest{Service: "fixture", IncludeToken: true, RefreshToken: true})
			require.NoError(t, err)
			require.Equal(t, ExternalAuthentication{Required: true, Method: "oauth", Token: "synthetic-fresh"}, got)
		})
	}
}

func TestExternalAuthenticationNamespaceFreeRefresh(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprint(force), func(t *testing.T) {
			token := selectedRefreshToken()
			token.AccessToken = "synthetic-before"
			if !force {
				token.ExpiresAt = time.Now().Add(-time.Hour).Unix()
			}
			store, registration := externalAuthTestStore(t, externalAuthTestPolicy(), token, func(registration *providerregistry.Registration) {
				registration.AccountNamespace = ""
			})
			store.config.authenticationBasis = newAuthenticationLoadBasis()
			_, _, err := loadFromConfigPathsObserved(t.Context(), lookupConfigsFromEnvironment(store.workingDir, store.baseEnvironment), nil, store.baseEnvironment, store.config.authenticationBasis)
			require.NoError(t, err)
			store.config.authenticationBasis.configuration(store.config)
			calls := 0
			store.exchangeToken = func(context.Context, string, string) (*oauth.Token, error) {
				calls++
				return selectedRefreshToken(), nil
			}
			got, err := store.ExternalAuthentication(t.Context(), ExternalAuthenticationRequest{Service: "fixture", IncludeToken: true, RefreshToken: force})
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, "synthetic-fresh", got.Token)
			data, err := os.ReadFile(store.globalDataPath)
			require.NoError(t, err)
			var saved Config
			require.NoError(t, json.Unmarshal(data, &saved))
			provider, ok := saved.Providers.Get(registration.ProviderID)
			require.True(t, ok)
			require.Equal(t, got.Token, provider.APIKey)
		})
	}
}
