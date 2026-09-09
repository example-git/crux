package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/example-git/crux/internal/lock"
)

// AcquireOperation serializes one operation's provider effect, durable result,
// and explicit abandonment across processes. The caller holds the lease until
// those effects finish, including a result-retention attempt after cancellation.
// Runtime cancellation stops acquisition; it does not release an acquired lease
// while its effect is still running. Process exit releases the OS file lock.
//
// Lock files are bounded to 256 buckets per journal kind. Hash collisions
// serialize unrelated operations. Kind separation permits a client-publication
// operation to contain a local-change operation without sharing its own bucket.
// This lease never holds the journal's global compare-and-swap lock.
func (journal AuthenticationJournal) AcquireOperation(ctx context.Context, key AuthenticationJournalKey) (func(), error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if journal.store == nil || !filepath.IsAbs(journal.path) || journal.scope == "" {
		return nil, errors.New("authentication operation journal is unavailable")
	}
	bound, cancel := journal.store.BindRuntimeContext(ctx)
	if err := bound.Err(); err != nil {
		cancel()
		return nil, err
	}
	if err := journal.store.RuntimeRevocation(); err != nil {
		cancel()
		return nil, err
	}
	directory := filepath.Join(journal.path+".operation-locks", key.Kind)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		cancel()
		return nil, authenticationInputError(err)
	}
	// id is the SHA-256 digest of the canonical, validated operation key.
	path := filepath.Join(directory, key.id()[:2]+".lock")
	release, err := lock.File(bound, path)
	if err != nil {
		cancel()
		return nil, authenticationInputError(err)
	}
	if err := bound.Err(); err != nil {
		release()
		cancel()
		return nil, err
	}
	if err := journal.store.RuntimeRevocation(); err != nil {
		release()
		cancel()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { release(); cancel() }) }, nil
}
