package imagegen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/providerregistry"
)

var ErrNoConfiguredCredentials = errors.New("no usable configured Codex or OpenAI API account; sign in to Codex or configure an OpenAI API account")

func NewProviderClient(store *config.ConfigStore) *Client {
	client := NewClient()
	if store != nil {
		client.authResolver = func(ctx context.Context) (resolvedAuth, error) {
			return resolveConfiguredAuth(ctx, store)
		}
	}
	return client
}

func resolveConfiguredAuth(ctx context.Context, store *config.ConfigStore) (resolvedAuth, error) {
	snapshot := store.RuntimeSnapshot()
	codexAuth, codexConfigured, codexErr := configuredCodexAuth(ctx, store, snapshot)
	if codexConfigured && codexErr == nil {
		return codexAuth, nil
	}
	openAIAuth, openAIConfigured, openAIErr := configuredOpenAIAuth(store, snapshot)
	if openAIConfigured && openAIErr == nil {
		return openAIAuth, nil
	}
	if codexErr != nil {
		return resolvedAuth{}, fmt.Errorf("resolve configured Codex account: %w", codexErr)
	}
	if openAIErr != nil {
		return resolvedAuth{}, fmt.Errorf("resolve configured OpenAI API account: %w", openAIErr)
	}
	return resolvedAuth{}, ErrNoConfiguredCredentials
}

func configuredCodexAuth(ctx context.Context, store *config.ConfigStore, snapshot config.RuntimeSnapshot) (resolvedAuth, bool, error) {
	cfg := snapshot.Config()
	if cfg == nil || cfg.Providers == nil {
		return resolvedAuth{}, false, nil
	}
	provider, ok := cfg.Providers.Get(codex.ID)
	if !ok || provider.Disable {
		return resolvedAuth{}, false, nil
	}
	registration, ok := snapshot.ProviderRegistrationFor(codex.ID, provider)
	if !ok || registration.ProviderID != codex.ID || registration.AccountNamespace != accounts.ProviderCodex || registration.Construction != providerregistry.ConstructionCodex {
		return resolvedAuth{}, false, nil
	}
	owner := registration.Owner()
	var refreshedToken string
	if provider.OAuthToken != nil && provider.OAuthToken.IsExpired() {
		if err := store.ValidateRegistrationOwner(owner); err != nil {
			return resolvedAuth{}, true, err
		}
		refreshed, err := store.RefreshOAuthTokenForOwner(ctx, config.ScopeGlobal, owner)
		if err != nil {
			return resolvedAuth{}, true, err
		}
		if err := store.ValidateRegistrationOwner(owner); err != nil {
			return resolvedAuth{}, true, err
		}
		refreshedToken = strings.TrimSpace(refreshed.AccessToken)
	}
	token, err := snapshot.Resolve(provider.APIKey)
	if err != nil {
		return resolvedAuth{}, true, err
	}
	if token = strings.TrimSpace(token); refreshedToken != "" {
		token = refreshedToken
	} else if token == "" && provider.OAuthToken != nil {
		token = strings.TrimSpace(provider.OAuthToken.AccessToken)
	}
	if token == "" {
		return resolvedAuth{}, true, errors.New("configured Codex account has no access token")
	}
	accountID, err := configuredCodexAccountID(ctx, store, snapshot, owner, token)
	if err != nil {
		return resolvedAuth{}, true, err
	}
	return resolvedAuth{
		mode:           AuthCodex,
		token:          token,
		accountID:      accountID,
		owner:          owner,
		ownerValidator: store.ValidateActiveProviderOwner,
	}, true, nil
}

func configuredOpenAIAuth(store *config.ConfigStore, snapshot config.RuntimeSnapshot) (resolvedAuth, bool, error) {
	cfg := snapshot.Config()
	if cfg == nil || cfg.Providers == nil {
		return resolvedAuth{}, false, nil
	}
	providerID := string(catalog.ProviderOpenAI)
	provider, ok := cfg.Providers.Get(providerID)
	if !ok || provider.Disable || provider.Plugin != nil || provider.Preset != nil {
		return resolvedAuth{}, false, nil
	}
	expected := providerregistry.RegistrationOwner{ProviderID: providerID}
	owner, active := snapshot.ProviderOwnerFor(providerID, provider)
	if !active {
		owner, active = snapshot.ProviderOwner(providerID)
	}
	if !active || owner != expected {
		return resolvedAuth{}, false, nil
	}
	token, err := snapshot.Resolve(provider.APIKey)
	if err != nil {
		return resolvedAuth{}, true, err
	}
	if token = strings.TrimSpace(token); token == "" {
		return resolvedAuth{}, true, errors.New("configured OpenAI API account has no API key")
	}
	baseURL := OpenAIBaseURL
	if strings.TrimSpace(provider.BaseURL) != "" {
		baseURL, err = snapshot.Resolve(provider.BaseURL)
		if err != nil {
			return resolvedAuth{}, true, err
		}
		if baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/"); baseURL == "" {
			return resolvedAuth{}, true, errors.New("configured OpenAI API account has no endpoint")
		}
	}
	return resolvedAuth{
		mode:           AuthAPIKey,
		token:          token,
		baseURL:        baseURL,
		owner:          owner,
		ownerValidator: store.ValidateActiveProviderOwner,
	}, true, nil
}

func configuredCodexAccountID(ctx context.Context, store *config.ConfigStore, snapshot config.RuntimeSnapshot, expected providerregistry.RegistrationOwner, token string) (string, error) {
	namespace := expected.AccountNamespace
	if entry, ok := snapshot.EphemeralAccount(expected); ok && entry != nil && entry.AccessToken == token {
		if accountID := accountIDFromRaw(entry.Raw); accountID != "" {
			return accountID, nil
		}
	}
	if err := store.ValidateRegistrationOwner(expected); err != nil {
		return "", err
	}
	entry, err := accounts.Active(ctx, namespace)
	if err != nil {
		return codex.AccountID(token), nil
	}
	if err := store.ValidateRegistrationOwner(expected); err != nil {
		return "", err
	}
	if entry != nil && entry.AccessToken == token {
		if accountID := accountIDFromRaw(entry.Raw); accountID != "" {
			return accountID, nil
		}
	}
	return codex.AccountID(token), nil
}

func accountIDFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value struct {
		AccountID string `json:"account_id"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value.AccountID
}
