package accounts

// Token refresh integration. Providers register a refresher at init time;
// AccessToken() then transparently refreshes and persists expired
// credentials for the active (or any specific) account.

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/oauth"
)

// Refresher exchanges a refresh token for a fresh token.
type Refresher func(ctx context.Context, refreshToken string) (*oauth.Token, error)

type Validator func() error

// RegisterRefresher registers the token refresher for a provider store key.
func RegisterRefresher(provider string, fn Refresher) {
	providerMu.Lock()
	defer providerMu.Unlock()
	refreshers[provider] = fn
}

// AccessToken returns a valid access token for the provider's active
// account, refreshing and persisting it first when expired. Returns "" when
// no account is stored.
func AccessToken(ctx context.Context, provider string) (string, error) {
	entry, err := Active(ctx, provider)
	if err != nil || entry == nil {
		return "", err
	}
	providerMu.RLock()
	refresher := refreshers[provider]
	providerMu.RUnlock()
	fresh, err := ensureFresh(ctx, provider, entry, true, refresher, nil)
	if err != nil {
		return "", err
	}
	return fresh.AccessToken, nil
}

// EnsureFresh refreshes the given account when expired, persisting the new
// credential without changing which account is active. Returns the fresh
// entry.
func EnsureFresh(ctx context.Context, provider string, entry *Entry) (*Entry, error) {
	providerMu.RLock()
	refresher := refreshers[provider]
	providerMu.RUnlock()
	return EnsureFreshWithRefresher(ctx, provider, entry, refresher)
}

func EnsureFreshWithRefresher(ctx context.Context, provider string, entry *Entry, refresher Refresher) (*Entry, error) {
	return ensureFresh(ctx, provider, entry, false, refresher, nil)
}

func EnsureFreshForOwner(ctx context.Context, provider string, entry *Entry, refresher Refresher, validate Validator) (*Entry, error) {
	return ensureFresh(ctx, provider, entry, false, refresher, validate)
}

func AccessTokenWithRefresher(ctx context.Context, provider string, refresher Refresher) (string, error) {
	return AccessTokenForOwner(ctx, provider, refresher, nil)
}

func AccessTokenForOwner(ctx context.Context, provider string, refresher Refresher, validate Validator) (string, error) {
	if validate != nil {
		if err := validate(); err != nil {
			return "", err
		}
	}
	entry, err := Active(ctx, provider)
	if err != nil || entry == nil {
		return "", err
	}
	fresh, err := ensureFresh(ctx, provider, entry, true, refresher, validate)
	if err != nil {
		return "", err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return "", err
		}
	}
	return fresh.AccessToken, nil
}

func ensureFresh(ctx context.Context, provider string, entry *Entry, activate bool, fn Refresher, validate Validator) (*Entry, error) {
	if entry == nil {
		return nil, errors.New("refresh requires an exact account")
	}
	if !entry.Expired() || entry.RefreshToken == "" {
		return entry, nil
	}
	return refreshAccount(ctx, provider, entry, fn, validate, activate, false)
}
