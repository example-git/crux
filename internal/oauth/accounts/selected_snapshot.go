package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
)

// RefreshSelectedForOwner retains this private snapshot's database path while
// applying the selected-account rotation rules. A proven peer rotation may be
// adopted; a manual replacement or selection change cannot authorize exchange.
func (before Snapshot) RefreshSelectedForOwner(ctx context.Context, namespace string, expected *Entry, refresher Refresher, validate Validator, force bool) (*Entry, error) {
	if err := before.validateSelectedNamespace(namespace); err != nil {
		return nil, err
	}
	if validate == nil {
		return nil, errors.New("account owner validator is required")
	}
	fresh, err := refreshAccountWithStorage(ctx, namespace, expected, refresher, validate, true, force, sharedOwnerRefresh, selectedAccountStorage{path: before.path})
	return fresh, privateSnapshotError(err)
}

// WithSelectedForOwner holds the captured database's account lease for a local
// config commit. The callback must not call account APIs or perform network I/O.
func (before Snapshot) WithSelectedForOwner(ctx context.Context, namespace string, expected Entry, validate Validator, commit func() error) error {
	if err := before.validateSelectedNamespace(namespace); err != nil {
		return err
	}
	if validate == nil || commit == nil {
		return errors.New("selected account commit requires owner validation and a callback")
	}
	storage := selectedAccountStorage{path: before.path}
	err := storage.withLock(ctx, func() error {
		if err := validate(); err != nil {
			return err
		}
		state, err := storage.read(ctx, namespace, expected.ID)
		if err != nil {
			return err
		}
		current := findActive(state, namespace)
		if current == nil || CredentialID(*current) != CredentialID(expected) {
			return ErrCredentialChanged
		}
		return commit()
	})
	return privateSnapshotError(err)
}

func (before Snapshot) validateSelectedNamespace(namespace string) error {
	if !before.valid || !filepath.IsAbs(before.path) || namespace == "" || !slices.Contains(before.namespaces, namespace) {
		return errors.New("selected account operation requires its captured database and namespace")
	}
	return nil
}

// An empty path retains the legacy backend. Only snapshot entry points supply a
// captured path; every operation for that path avoids process environment.
type selectedAccountStorage struct{ path string }

func (storage selectedAccountStorage) root() (string, error) {
	if storage.path == "" {
		return dir()
	}
	return filepath.Dir(storage.path), nil
}

func (storage selectedAccountStorage) withLock(ctx context.Context, fn func() error) error {
	if storage.path == "" {
		return withLock(ctx, fn)
	}
	return withResolvedLock(ctx, func() (string, error) { return storage.path + ".lock", nil }, fn)
}

func (storage selectedAccountStorage) read(ctx context.Context, namespace, accountID string) (*store, error) {
	if storage.path == "" {
		return readStore()
	}
	snapshot, err := captureStateAtLocked(ctx, storage.path, []string{namespace})
	if err != nil {
		return nil, privateSnapshotError(err)
	}
	state, err := selectedStoreDocument(snapshot.document)
	if err != nil {
		return nil, err
	}
	if find(state.Accounts[namespace], accountID) != nil {
		// Establish the exact target's writable shape before consuming a rotating
		// token. Legacy JSON decoding alone accepts ambiguous fields and IDs that
		// the protected writer must reject. The staged bytes are never committed.
		target, err := readInactiveRefreshTarget(snapshot.document, namespace, accountID)
		if err != nil {
			return nil, err
		}
		if _, err := stageInactiveRefresh(snapshot.document, namespace, accountID, target, target.entry); err != nil {
			return nil, err
		}
	}
	return state, nil
}

func selectedStoreDocument(document []byte) (*store, error) {
	state := emptyStore()
	if len(document) != 0 {
		if err := json.Unmarshal(document, state); err != nil {
			return nil, errors.New("invalid captured account database")
		}
		if state.Active == nil {
			state.Active = make(map[string]string)
		}
		if state.Accounts == nil {
			state.Accounts = make(map[string][]Entry)
		}
	}
	return state, nil
}

func (storage selectedAccountStorage) saveRefresh(ctx context.Context, validate Validator, namespace, accountID string, fresh Entry, mutate func(*store) error) (bool, error) {
	if storage.path == "" {
		err := mutateStore(ctx, validate, mutate)
		return err == nil, err
	}
	if err := validate(); err != nil {
		return false, err
	}
	release, err := acquireResolvedLock(ctx, func() (string, error) { return storage.path + ".lock", nil })
	if err != nil {
		return false, privateSnapshotError(err)
	}
	change := &PendingChange{state: &pendingAccountChange{kind: accountRefresh, release: release, validate: validate}}
	defer change.Close()
	if err := validate(); err != nil {
		return false, err
	}
	current, err := captureStateAtLocked(ctx, storage.path, []string{namespace})
	if err != nil {
		return false, privateSnapshotError(err)
	}
	state, err := selectedStoreDocument(current.document)
	if err != nil {
		return false, err
	}
	// Preserve the same credential/mutation/selection CAS as legacy refresh.
	if err := mutate(state); err != nil {
		return false, err
	}
	target, err := readInactiveRefreshTarget(current.document, namespace, accountID)
	if err != nil {
		return false, err
	}
	// This shared staging function changes only the exact account's token fields
	// and rotation chain. Foreign fields and unrelated namespaces retain bytes.
	staged, err := stageInactiveRefresh(current.document, namespace, accountID, target, fresh)
	if err != nil {
		return false, err
	}
	change.state.before, change.state.staged = current, staged
	result, err := change.Commit(ctx)
	return result.Written, err
}
