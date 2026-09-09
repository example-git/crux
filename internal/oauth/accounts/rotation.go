package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/providertransport"
)

type refreshAuthority uint8

const (
	// Registered and exact-owner consumers may share a proven stored rotation.
	sharedOwnerRefresh refreshAuthority = iota
	// An unqualified explicit refresher is the caller's chosen exchange source.
	explicitRefresher
)

var ErrCredentialChanged = errors.New("account or credential changed during refresh")

type rotation struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

func (s *store) markMutation(provider, id string) {
	if s.Mutations == nil {
		s.Mutations = map[string]map[string]uint64{}
	}
	if s.Mutations[provider] == nil {
		s.Mutations[provider] = map[string]uint64{}
	}
	s.Mutations[provider][id]++
}

func (s *store) markSelection(provider, id string) {
	if s.Active[provider] == id {
		return
	}
	if s.Selections == nil {
		s.Selections = map[string]uint64{}
	}
	s.Selections[provider]++
}

// CredentialID compares exact account credentials and identity metadata without
// exporting token values. JSON whitespace in saved metadata is not identity.
func CredentialID(entry Entry) string {
	var raw any
	if len(entry.Raw) > 0 {
		if err := json.Unmarshal(entry.Raw, &raw); err != nil {
			raw = string(entry.Raw)
		}
	}
	data, _ := json.Marshal([]any{entry.ID, entry.AccessToken, entry.RefreshToken, entry.ExpiresAt, raw})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RefreshSelectedForOwner refreshes precisely the selected account. A peer's
// completed rotation can be adopted, but manual replacement, account switching
// and removal are never interpreted as refresh success.
func RefreshSelectedForOwner(ctx context.Context, provider string, entry *Entry, refresher Refresher, validate Validator, force bool) (*Entry, error) {
	if validate == nil {
		return nil, errors.New("account owner validator is required")
	}
	return refreshAccount(ctx, provider, entry, refresher, validate, true, force, sharedOwnerRefresh)
}

// WithSelectedForOwner runs a local commit while the exact account selection
// cannot change. The callback must not call accounts APIs or perform network IO.
func WithSelectedForOwner(ctx context.Context, provider string, expected Entry, validate Validator, commit func() error) error {
	if validate == nil || commit == nil {
		return errors.New("selected account commit requires owner validation and a callback")
	}
	return withLock(ctx, func() error {
		if err := validate(); err != nil {
			return err
		}
		s, err := readStore()
		if err != nil {
			return err
		}
		current := findActive(s, provider)
		if current == nil || CredentialID(*current) != CredentialID(expected) {
			return ErrCredentialChanged
		}
		return commit()
	})
}

func rotationDescends(s *store, provider, id, before, after string) bool {
	if before == after {
		return true
	}
	current := before
	for _, link := range s.Rotations[provider][id] {
		if link.Before == current {
			current = link.After
		}
	}
	return current == after
}

func refreshAccount(ctx context.Context, provider string, expected *Entry, refresher Refresher, validate Validator, selected, force bool, authority refreshAuthority) (*Entry, error) {
	if expected == nil || expected.ID == "" || provider == "" {
		return nil, errors.New("refresh requires an exact account")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return nil, err
		}
	}
	root, err := dir()
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(provider + "\x00" + expected.ID))
	path := filepath.Join(root, "locks", fmt.Sprintf("%x.account-refresh.lock", digest))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	release, err := lock.File(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("acquire account refresh lock: %w", err)
	}
	defer release()

	expectedID := CredentialID(*expected)
	var current Entry
	var adopted bool
	var mutation, selection uint64
	err = withLock(ctx, func() error {
		s, err := readStore()
		if err != nil {
			return err
		}
		entry := find(s.Accounts[provider], expected.ID)
		if entry == nil || selected && s.Active[provider] != expected.ID {
			return ErrCredentialChanged
		}
		current = *entry
		mutation, selection = s.Mutations[provider][expected.ID], s.Selections[provider]
		currentID := CredentialID(current)
		if currentID != expectedID {
			if current.Expired() || !rotationDescends(s, provider, expected.ID, expectedID, currentID) {
				return ErrCredentialChanged
			}
			adopted = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return nil, err
		}
	}
	exchangeSuccessor := adopted && authority == explicitRefresher
	if !exchangeSuccessor && (adopted || !force && !current.Expired()) {
		return &current, nil
	}
	// The preceding rotation consumed the original refresh token. Retain the
	// same account lease and exchange only its proven current descendant. The
	// persistence CAS/history must now bind that descendant, not the old input.
	if exchangeSuccessor {
		expectedID = CredentialID(current)
	}
	if current.RefreshToken == "" || refresher == nil {
		return nil, errors.New("selected account has no refresh capability; sign in again")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Once the exchange begins, remote disconnection must not discard a rotated
	// token. Finish a bounded local commit, still conditional on account/owner
	// identity. Cancellation before this point prevents the exchange entirely.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	exchangeCtx := commitCtx
	if validate != nil {
		exchangeCtx = providertransport.ContextWithOwnerValidator(commitCtx, providertransport.OwnerValidator(validate))
	}
	token, err := refresher(exchangeCtx, current.RefreshToken)
	if err != nil {
		return nil, err
	}
	if token == nil || token.AccessToken == "" {
		return nil, errors.New("account refresh returned no access token")
	}
	if validate != nil {
		if err := validate(); err != nil {
			return nil, err
		}
	}
	fresh := FromToken(current.ID, current.DisplayName, token, &current)
	registerSecrets(fresh)
	if validate != nil {
		if err := validate(); err != nil {
			return nil, err
		}
	}
	err = mutateStore(commitCtx, validate, func(s *store) error {
		entry := find(s.Accounts[provider], current.ID)
		if entry == nil || CredentialID(*entry) != expectedID || s.Mutations[provider][current.ID] != mutation || selected && (s.Active[provider] != current.ID || s.Selections[provider] != selection) {
			return ErrCredentialChanged
		}
		s.Accounts[provider] = upsert(s.Accounts[provider], fresh)
		if s.Rotations == nil {
			s.Rotations = map[string]map[string][]rotation{}
		}
		if s.Rotations[provider] == nil {
			s.Rotations[provider] = map[string][]rotation{}
		}
		history := append(s.Rotations[provider][current.ID], rotation{Before: expectedID, After: CredentialID(fresh)})
		// Old requests beyond this bounded history fail visibly and recollect;
		// they never guess whether an unrelated replacement was a rotation.
		if len(history) > 8 {
			history = history[len(history)-8:]
		}
		s.Rotations[provider][current.ID] = history
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}
