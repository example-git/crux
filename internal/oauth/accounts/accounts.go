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
	"github.com/example-git/crux/internal/redact"
)

// Provider store keys. These are the cross-tool provider namespaces used in
// accounts.json, which differ from Crux's provider IDs.
const (
	ProviderCodex   = "codex"
	ProviderCopilot = "copilot"
	ProviderGemini  = "gemini"
)

type ProviderRegistration struct {
	ProviderID string
	Namespace  string
	Aliases    []string
	Order      int
	Refresher  Refresher
}

type providerRegistration struct {
	providerID string
	namespace  string
	aliases    []string
	order      int
}

var (
	providerMu    sync.RWMutex
	providersByID = map[string]providerRegistration{}
	refreshers    = map[string]Refresher{}
)

func PublishProviders(registrations []ProviderRegistration) {
	PublishProvidersWith(registrations, nil)
}

func PublishProvidersWith(registrations []ProviderRegistration, publish func()) {
	_ = PublishProvidersTransaction(registrations, func() error {
		if publish != nil {
			publish()
		}
		return nil
	})
}

func PublishProvidersTransaction(registrations []ProviderRegistration, publish func() error) error {
	providers := make(map[string]providerRegistration, len(registrations))
	providerRefreshers := make(map[string]Refresher, len(registrations))
	for _, registration := range registrations {
		if registration.ProviderID == "" || registration.Namespace == "" {
			continue
		}
		cleanAliases := make([]string, 0, len(registration.Aliases))
		for _, alias := range registration.Aliases {
			alias = strings.TrimSpace(alias)
			if alias != "" && alias != registration.Namespace && !slices.Contains(cleanAliases, alias) {
				cleanAliases = append(cleanAliases, alias)
			}
		}
		providers[registration.ProviderID] = providerRegistration{providerID: registration.ProviderID, namespace: registration.Namespace, aliases: cleanAliases, order: registration.Order}
		if registration.Refresher != nil {
			providerRefreshers[registration.Namespace] = registration.Refresher
		}
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	if publish != nil {
		if err := publish(); err != nil {
			return err
		}
	}
	providersByID = providers
	refreshers = providerRefreshers
	return nil
}

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

func ProviderSnapshot(providerID string) (string, Refresher, bool) {
	providerMu.RLock()
	defer providerMu.RUnlock()
	registration, ok := providersByID[providerID]
	if !ok {
		return "", nil, false
	}
	return registration.namespace, refreshers[registration.namespace], true
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

func registerSecrets(entry Entry) {
	redact.Register(entry.AccessToken, entry.RefreshToken)
	redact.RegisterJSONBytes(entry.Raw)
}

func writeStore(s *store, validate Validator) error {
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
	if validate != nil {
		if err := validate(); err != nil {
			if removeErr := os.Remove(tmp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return errors.Join(err, fmt.Errorf("remove rejected account store temporary file: %w", removeErr))
			}
			return err
		}
	}
	return os.Rename(tmp, path)
}

func mutateStore(ctx context.Context, validate Validator, mutate func(*store) error) error {
	if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	return withLock(ctx, func() error {
		if validate != nil {
			if err := validate(); err != nil {
				return err
			}
		}
		s, err := readStore()
		if err != nil {
			return err
		}
		if err := mutate(s); err != nil {
			return err
		}
		if validate != nil {
			if err := validate(); err != nil {
				return err
			}
		}
		return writeStore(s, validate)
	})
}

// Save upserts an account and makes it the active account for the provider.
func Save(ctx context.Context, provider string, entry Entry) error {
	return save(ctx, provider, entry, nil)
}

func SaveForOwner(ctx context.Context, provider string, entry Entry, validate Validator) error {
	if validate == nil {
		return errors.New("account owner validator is required")
	}
	return save(ctx, provider, entry, validate)
}

func save(ctx context.Context, provider string, entry Entry, validate Validator) error {
	registerSecrets(entry)
	return mutateStore(ctx, validate, func(s *store) error {
		s.Accounts[provider] = upsert(s.Accounts[provider], entry)
		s.Active[provider] = entry.ID
		return nil
	})
}

// SaveWithoutActivating upserts an account without changing which account is
// active. Used for background refreshes of inactive accounts.
func SaveWithoutActivating(ctx context.Context, provider string, entry Entry) error {
	return saveWithoutActivating(ctx, provider, entry, nil)
}

func SaveWithoutActivatingForOwner(ctx context.Context, provider string, entry Entry, validate Validator) error {
	if validate == nil {
		return errors.New("account owner validator is required")
	}
	return saveWithoutActivating(ctx, provider, entry, validate)
}

func saveWithoutActivating(ctx context.Context, provider string, entry Entry, validate Validator) error {
	registerSecrets(entry)
	return mutateStore(ctx, validate, func(s *store) error {
		s.Accounts[provider] = upsert(s.Accounts[provider], entry)
		return nil
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
	for _, entry := range out {
		registerSecrets(entry)
	}
	return out, err
}

// Providers returns the provider keys that have at least one stored account,
// in preference order followed by any unknown keys.
func Providers(ctx context.Context) ([]string, error) {
	return providers(ctx, providerOrder())
}

func ProvidersFor(ctx context.Context, preferred []string) ([]string, error) {
	return providers(ctx, preferred)
}

func providers(ctx context.Context, preferred []string) ([]string, error) {
	var out []string
	err := withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, provider := range preferred {
			if provider == "" || seen[provider] {
				continue
			}
			seen[provider] = true
			if len(s.Accounts[provider]) > 0 {
				out = append(out, provider)
			}
		}
		var remaining []string
		for provider, entries := range s.Accounts {
			if !seen[provider] && len(entries) > 0 {
				remaining = append(remaining, provider)
			}
		}
		slices.Sort(remaining)
		out = append(out, remaining...)
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
	if out != nil {
		registerSecrets(*out)
	}
	return out, err
}

// SetActive marks the account with the given id active for the provider.
func SetActive(ctx context.Context, provider, id string) error {
	return setActive(ctx, provider, id, nil)
}

func SetActiveForOwner(ctx context.Context, provider, id string, validate Validator) error {
	if validate == nil {
		return errors.New("account owner validator is required")
	}
	return setActive(ctx, provider, id, validate)
}

func setActive(ctx context.Context, provider, id string, validate Validator) error {
	return mutateStore(ctx, validate, func(s *store) error {
		if find(s.Accounts[provider], id) == nil {
			return fmt.Errorf("account %q not found for provider %q", id, provider)
		}
		s.Active[provider] = id
		return nil
	})
}

// RemoveProvider deletes every account and active selection for a provider.
func RemoveProvider(ctx context.Context, provider string) error {
	return removeProvider(ctx, provider, nil)
}

func RemoveProviderForOwner(ctx context.Context, provider string, validate Validator) error {
	if validate == nil {
		return errors.New("account owner validator is required")
	}
	return removeProvider(ctx, provider, validate)
}

func removeProvider(ctx context.Context, provider string, validate Validator) error {
	return mutateStore(ctx, validate, func(s *store) error {
		delete(s.Accounts, provider)
		delete(s.Active, provider)
		return nil
	})
}

// Remove deletes an account. If it was active, the first remaining account
// becomes active (or the active key is cleared).
func Remove(ctx context.Context, provider, id string) error {
	return remove(ctx, provider, id, nil)
}

func RemoveForOwner(ctx context.Context, provider, id string, validate Validator) error {
	if validate == nil {
		return errors.New("account owner validator is required")
	}
	return remove(ctx, provider, id, validate)
}

func remove(ctx context.Context, provider, id string, validate Validator) error {
	return mutateStore(ctx, validate, func(s *store) error {
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
		return nil
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
