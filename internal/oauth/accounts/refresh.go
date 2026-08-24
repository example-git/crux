package accounts

// Token refresh integration. Providers register a refresher at init time;
// AccessToken() then transparently refreshes and persists expired
// credentials for the active (or any specific) account.

import (
	"context"
	"fmt"
	"sync"

	"github.com/example-git/crux/internal/oauth"
)

// Refresher exchanges a refresh token for a fresh token.
type Refresher func(ctx context.Context, refreshToken string) (*oauth.Token, error)

var (
	refreshMu  sync.RWMutex
	refreshers = map[string]Refresher{}
	// refreshSF single-flights refreshes per provider/account so concurrent
	// callers do not race the same rotated refresh token.
	refreshSFMu sync.Mutex
	refreshSF   = map[string]*refreshCall{}
)

type refreshCall struct {
	done  chan struct{}
	entry *Entry
	err   error
}

// RegisterRefresher registers the token refresher for a provider store key.
func RegisterRefresher(provider string, fn Refresher) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
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
	fresh, err := ensureFresh(ctx, provider, entry, true)
	if err != nil {
		return "", err
	}
	return fresh.AccessToken, nil
}

// EnsureFresh refreshes the given account when expired, persisting the new
// credential without changing which account is active. Returns the fresh
// entry.
func EnsureFresh(ctx context.Context, provider string, entry *Entry) (*Entry, error) {
	return ensureFresh(ctx, provider, entry, false)
}

func ensureFresh(ctx context.Context, provider string, entry *Entry, activate bool) (*Entry, error) {
	if !entry.Expired() || entry.RefreshToken == "" {
		return entry, nil
	}

	refreshMu.RLock()
	fn := refreshers[provider]
	refreshMu.RUnlock()
	if fn == nil {
		return nil, fmt.Errorf("no token refresher registered for provider %q", provider)
	}

	key := provider + "\x00" + entry.ID

	refreshSFMu.Lock()
	if call, ok := refreshSF[key]; ok {
		refreshSFMu.Unlock()
		select {
		case <-call.done:
			return call.entry, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	refreshSF[key] = call
	refreshSFMu.Unlock()

	defer func() {
		close(call.done)
		refreshSFMu.Lock()
		delete(refreshSF, key)
		refreshSFMu.Unlock()
	}()

	// Re-read from disk under the single flight: another process may have
	// already refreshed and rotated the token.
	if current, err := findByID(ctx, provider, entry.ID); err == nil && current != nil {
		if !current.Expired() {
			call.entry = current
			return current, nil
		}
		entry = current
	}

	token, err := fn(ctx, entry.RefreshToken)
	if err != nil {
		call.err = err
		return nil, err
	}
	fresh := FromToken(entry.ID, entry.DisplayName, token, entry)
	if activate {
		err = Save(ctx, provider, fresh)
	} else {
		err = SaveWithoutActivating(ctx, provider, fresh)
	}
	if err != nil {
		call.err = err
		return nil, err
	}
	call.entry = &fresh
	return &fresh, nil
}

// findByID returns a specific account by id, or nil.
func findByID(ctx context.Context, provider, id string) (*Entry, error) {
	list, err := List(ctx, provider)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, nil
}
