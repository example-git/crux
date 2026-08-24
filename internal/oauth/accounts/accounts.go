// Package accounts is a multi-account OAuth credential store shared across
// providers. It persists to ~/.ai-cli/accounts.json using the same schema as
// the reference TUI implementation, so accounts saved by either tool are
// visible to both:
//
//	{
//	  "active":   { "codex": "user@example.com", ... },
//	  "accounts": { "codex": [ { "id": ..., "accessToken": ..., ... } ] }
//	}
//
// Each provider holds any number of accounts; one per provider is active.
// Token refresh goes through a per-provider refresher registry so the store
// can transparently refresh and persist expired credentials.
package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth"
)

// Provider store keys. These are the cross-tool provider namespaces used in
// accounts.json, which differ from Crux's provider IDs.
const (
	ProviderCodex   = "codex"
	ProviderCopilot = "copilot"
	ProviderGemini  = "gemini"
)

type providerRegistration struct {
	providerID string
	namespace  string
	aliases    []string
	order      int
}

var (
	providerMu    sync.RWMutex
	providersByID = map[string]providerRegistration{}
)

// RegisterProvider registers the stable account namespace for one logical
// provider. Re-registering the same mapping is idempotent; conflicting claims
// are ignored so a plugin cannot displace another provider's account records.
func RegisterProvider(providerID, namespace string, order int, aliases ...string) {
	if providerID == "" || namespace == "" {
		return
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	if current, ok := providersByID[providerID]; ok && current.namespace != namespace {
		return
	}
	for id, current := range providersByID {
		if id != providerID && current.namespace == namespace {
			return
		}
	}
	cleanAliases := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias != "" && alias != namespace && !slices.Contains(cleanAliases, alias) {
			cleanAliases = append(cleanAliases, alias)
		}
	}
	providersByID[providerID] = providerRegistration{providerID: providerID, namespace: namespace, aliases: cleanAliases, order: order}
}

// StoreKey maps a Crux provider ID to its registered accounts.json namespace.
// Returns "" for providers without OAuth multi-account support.
func StoreKey(providerID string) string {
	providerMu.RLock()
	defer providerMu.RUnlock()
	return providersByID[providerID].namespace
}

// ProviderID maps an accounts.json namespace back to its logical provider ID.
// Unknown namespaces are returned unchanged so orphaned account data remains
// visible when the corresponding plugin is absent.
func ProviderID(namespace string) string {
	providerMu.RLock()
	defer providerMu.RUnlock()
	for id, registration := range providersByID {
		if registration.namespace == namespace || slices.Contains(registration.aliases, namespace) {
			return id
		}
	}
	return namespace
}

func providerOrder() []string {
	providerMu.RLock()
	registrations := make([]providerRegistration, 0, len(providersByID))
	for _, registration := range providersByID {
		registrations = append(registrations, registration)
	}
	providerMu.RUnlock()
	slices.SortFunc(registrations, func(a, b providerRegistration) int {
		if a.order != b.order {
			return a.order - b.order
		}
		return strings.Compare(a.namespace, b.namespace)
	})
	result := make([]string, 0, len(registrations))
	seen := map[string]bool{}
	for _, registration := range registrations {
		if !seen[registration.namespace] {
			result = append(result, registration.namespace)
			seen[registration.namespace] = true
		}
	}
	return result
}

// Entry is one stored account.
type Entry struct {
	ID           string `json:"id"`
	DisplayName  string `json:"displayName"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	// ExpiresAt is epoch milliseconds, matching the reference schema.
	ExpiresAt int64           `json:"expiresAt,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// Expired reports whether the access token is expired (with a 30s buffer).
// Entries without expiry information are never considered expired.
func (e *Entry) Expired() bool {
	if e.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(30*time.Second).UnixMilli() >= e.ExpiresAt
}

// Token converts the entry to an oauth.Token.
func (e *Entry) Token() *oauth.Token {
	t := &oauth.Token{
		AccessToken:  e.AccessToken,
		RefreshToken: e.RefreshToken,
	}
	if e.ExpiresAt > 0 {
		t.ExpiresAt = e.ExpiresAt / 1000
	}
	return t
}

// FromToken builds an entry from an oauth.Token, preserving id/display/raw
// from prev when provided.
func FromToken(id, displayName string, token *oauth.Token, prev *Entry) Entry {
	entry := Entry{
		ID:           id,
		DisplayName:  displayName,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
	}
	if token.ExpiresAt > 0 {
		entry.ExpiresAt = token.ExpiresAt * 1000
	}
	if prev != nil {
		if entry.ID == "" {
			entry.ID = prev.ID
		}
		if entry.DisplayName == "" {
			entry.DisplayName = prev.DisplayName
		}
		if entry.RefreshToken == "" {
			entry.RefreshToken = prev.RefreshToken
		}
		entry.Raw = prev.Raw
	}
	return entry
}

// store is the on-disk schema.
type store struct {
	Active   map[string]string  `json:"active"`
	Accounts map[string][]Entry `json:"accounts"`
}

func emptyStore() *store {
	return &store{Active: map[string]string{}, Accounts: map[string][]Entry{}}
}

// dir returns the ai-cli data dir, honoring the AI_CLI_DIR override used by
// the reference implementation.
func dir() (string, error) {
	if v := os.Getenv("AI_CLI_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ai-cli"), nil
}

// Path returns the authoritative account-store path used by migration and
// backup tooling. Callers must still use account APIs for normal mutations.
func Path() (string, error) {
	return dbPath()
}

func dbPath() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "accounts.json"), nil
}

func lockPath() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "accounts.json.lock"), nil
}

var mu sync.Mutex

// withLock runs fn holding both the in-process mutex and the cross-process
// file lock.
func withLock(ctx context.Context, fn func() error) error {
	mu.Lock()
	defer mu.Unlock()
	lp, err := lockPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lp), 0o700); err != nil {
		return err
	}
	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	release, err := lock.File(lockCtx, lp)
	if err != nil {
		return fmt.Errorf("acquire accounts lock: %w", err)
	}
	defer release()
	return fn()
}

func readStore() (*store, error) {
	path, err := dbPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyStore(), nil
		}
		return nil, fmt.Errorf("read account store: %w", err)
	}
	var s store
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse account store at %s: %w", path, err)
	}
	if s.Active == nil {
		s.Active = map[string]string{}
	}
	if s.Accounts == nil {
		s.Accounts = map[string][]Entry{}
	}
	return &s, nil
}

func writeStore(s *store) error {
	path, err := dbPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Save upserts an account and makes it the active account for the provider.
func Save(ctx context.Context, provider string, entry Entry) error {
	return withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		s.Accounts[provider] = upsert(s.Accounts[provider], entry)
		s.Active[provider] = entry.ID
		return writeStore(s)
	})
}

// SaveWithoutActivating upserts an account without changing which account is
// active. Used for background refreshes of inactive accounts.
func SaveWithoutActivating(ctx context.Context, provider string, entry Entry) error {
	return withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		s.Accounts[provider] = upsert(s.Accounts[provider], entry)
		return writeStore(s)
	})
}

// List returns all accounts for a provider.
func List(ctx context.Context, provider string) ([]Entry, error) {
	var out []Entry
	err := withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		out = append(out, s.Accounts[provider]...)
		return nil
	})
	return out, err
}

// Providers returns the provider keys that have at least one stored account,
// in preference order followed by any unknown keys.
func Providers(ctx context.Context) ([]string, error) {
	var out []string
	err := withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, p := range providerOrder() {
			if len(s.Accounts[p]) > 0 {
				out = append(out, p)
				seen[p] = true
			}
		}
		for p, list := range s.Accounts {
			if !seen[p] && len(list) > 0 {
				out = append(out, p)
			}
		}
		return nil
	})
	return out, err
}

// Active returns the active account for a provider, or nil.
func Active(ctx context.Context, provider string) (*Entry, error) {
	var out *Entry
	err := withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		out = findActive(s, provider)
		return nil
	})
	return out, err
}

// SetActive marks the account with the given id active for the provider.
func SetActive(ctx context.Context, provider, id string) error {
	return withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		if find(s.Accounts[provider], id) == nil {
			return fmt.Errorf("account %q not found for provider %q", id, provider)
		}
		s.Active[provider] = id
		return writeStore(s)
	})
}

// RemoveProvider deletes every account and active selection for a provider.
func RemoveProvider(ctx context.Context, provider string) error {
	return withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		delete(s.Accounts, provider)
		delete(s.Active, provider)
		return writeStore(s)
	})
}

// Remove deletes an account. If it was active, the first remaining account
// becomes active (or the active key is cleared).
func Remove(ctx context.Context, provider, id string) error {
	return withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		list := s.Accounts[provider]
		filtered := list[:0:0]
		for _, e := range list {
			if e.ID != id {
				filtered = append(filtered, e)
			}
		}
		s.Accounts[provider] = filtered
		if s.Active[provider] == id {
			if len(filtered) > 0 {
				s.Active[provider] = filtered[0].ID
			} else {
				delete(s.Active, provider)
			}
		}
		return writeStore(s)
	})
}

func upsert(list []Entry, entry Entry) []Entry {
	for i, e := range list {
		if e.ID == entry.ID {
			list[i] = entry
			return list
		}
	}
	return append(list, entry)
}

func find(list []Entry, id string) *Entry {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func findActive(s *store, provider string) *Entry {
	id := s.Active[provider]
	if id == "" {
		return nil
	}
	if e := find(s.Accounts[provider], id); e != nil {
		out := *e
		return &out
	}
	return nil
}
