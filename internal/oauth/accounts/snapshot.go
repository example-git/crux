package accounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// Snapshot is an immutable host-private account observation. It must never be
// used as a wire DTO or as a public generation identifier. Whole-file changes
// conservatively invalidate all observations, including unrelated namespaces.
type Snapshot struct {
	valid      bool
	path       string
	namespaces []string
	file       accountFileObservation
	content    [sha256.Size]byte
	// Retain the original document privately so fixed conditional changes can
	// preserve foreign harness fields and untouched entry/namespace bytes.
	document []byte
	entries  map[string][]Entry
	active   map[string]string
}

type accountFileObservation struct {
	exists   bool
	identity [sha256.Size]byte
	size     int64
	mode     os.FileMode
	modified int64
}

// EmptySnapshot represents a capture with no account namespaces. It performs no
// path lookup or I/O and permits only the no-op check lifecycle. Config-only
// authentication can use its observation without acquiring account authority.
func EmptySnapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{valid: true}, nil
}

func (s Snapshot) empty() bool { return s.valid && s.path == "" && len(s.namespaces) == 0 }

func (Snapshot) String() string   { return "accounts.Snapshot(private)" }
func (Snapshot) GoString() string { return "accounts.Snapshot(private)" }
func (Snapshot) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "accounts.Snapshot(private)")
}

func (Snapshot) MarshalJSON() ([]byte, error) {
	return nil, errors.New("account snapshots are private and cannot be serialized")
}

// SameObservation compares private captures, including the captured path,
// requested namespace set, stable file identity/metadata and complete contents.
// It detects observed changes, not arbitrary unobserved out-of-protocol ABA.
func (s Snapshot) SameObservation(other Snapshot) bool {
	return s.valid && other.valid && s.path == other.path &&
		slices.Equal(s.namespaces, other.namespaces) && s.file == other.file && s.content == other.content
}

// Entries returns host-only credentials for an exact requested namespace. Both
// the returned slice and each entry's Raw data are independent of the snapshot.
func (s Snapshot) Entries(namespace string) []Entry {
	return cloneSnapshotEntries(s.entries[namespace])
}

// ActiveID returns the active ID of an exact requested namespace.
func (s Snapshot) ActiveID(namespace string) string { return s.active[namespace] }

func cloneSnapshotEntries(entries []Entry) []Entry {
	cloned := slices.Clone(entries)
	for i := range cloned {
		cloned[i].Raw = bytes.Clone(cloned[i].Raw)
	}
	return cloned
}

// CaptureStateAt reads the supplied absolute account database path under the
// ordinary account mutex and its cross-process lock. It never consults live
// environment variables. The caller must reject detached receiver runtimes
// before calling: acquiring the lock may create its directory and lock file.
func CaptureStateAt(ctx context.Context, path string, namespaces []string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if !filepath.IsAbs(path) {
		return Snapshot{}, errors.New("account snapshot requires an absolute database path")
	}
	path = filepath.Clean(path)
	namespaces = slices.Clone(namespaces)
	slices.Sort(namespaces)
	namespaces = slices.Compact(namespaces)
	var snapshot Snapshot
	err := withResolvedLock(ctx, func() (string, error) { return path + ".lock", nil }, func() error {
		var err error
		snapshot, err = captureStateAtLocked(ctx, path, namespaces)
		return err
	})
	if err != nil {
		return Snapshot{}, privateSnapshotError(err)
	}
	return snapshot, nil
}

func captureStateAtLocked(ctx context.Context, path string, namespaces []string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	file, err := openAccountSnapshotFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, errors.New("account database changed during capture")
		}
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		return snapshotFromStore(path, namespaces, accountFileObservation{}, nil, emptyStore()), nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	defer file.Close()
	before, err := observeAccountFile(file)
	if err != nil {
		return Snapshot{}, err
	}
	data, err := io.ReadAll(snapshotReader{ctx: ctx, reader: file})
	if err != nil {
		return Snapshot{}, err
	}
	if err := verifyAccountFile(ctx, path, file, before); err != nil {
		return Snapshot{}, err
	}
	var state store
	if err := json.Unmarshal(data, &state); err != nil {
		// Parser errors can include private field values. Keep them off callers'
		// error paths as well as the eventual public status boundary.
		return Snapshot{}, errors.New("invalid account database")
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return snapshotFromStore(path, namespaces, before, data, &state), nil
}

func snapshotFromStore(path string, namespaces []string, file accountFileObservation, data []byte, state *store) Snapshot {
	snapshot := Snapshot{
		valid: true, path: path, namespaces: slices.Clone(namespaces), file: file,
		content: sha256.Sum256(data), document: bytes.Clone(data), entries: make(map[string][]Entry, len(namespaces)), active: make(map[string]string, len(namespaces)),
	}
	for _, namespace := range namespaces {
		snapshot.entries[namespace] = cloneSnapshotEntries(state.Accounts[namespace])
		snapshot.active[namespace] = state.Active[namespace]
		for _, entry := range snapshot.entries[namespace] {
			registerSecrets(entry)
		}
	}
	return snapshot
}

func observeAccountFile(file *os.File) (accountFileObservation, error) {
	info, err := file.Stat()
	if err != nil {
		return accountFileObservation{}, err
	}
	if !info.Mode().IsRegular() {
		return accountFileObservation{}, errors.New("account database is not a regular file")
	}
	identity, err := accountFileIdentity(file, info)
	if err != nil {
		return accountFileObservation{}, err
	}
	return accountFileObservation{exists: true, identity: identity, size: info.Size(), mode: info.Mode(), modified: info.ModTime().UnixNano()}, nil
}

// verifyAccountFile rechecks the open descriptor and the current target of the
// captured path while holding the advisory lock. Uncoordinated replacement or
// in-place changes during the read fail visibly instead of returning torn data.
func verifyAccountFile(ctx context.Context, path string, file *os.File, expected accountFileObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := observeAccountFile(file)
	if err != nil {
		return err
	}
	if current != expected {
		return errors.New("account database changed during capture")
	}
	target, err := openAccountSnapshotFile(path)
	if err != nil {
		return err
	}
	defer target.Close()
	actual, err := observeAccountFile(target)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("account database changed during capture")
	}
	return ctx.Err()
}

func privateSnapshotError(err error) error {
	// Do not include captured host paths in errors. Preserve cancellation and
	// useful filesystem error identities for callers without exposing names.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("capture account database: %w", pathErr.Err)
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return fmt.Errorf("commit account database: %w", linkErr.Err)
	}
	return err
}

type snapshotReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r snapshotReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

// accountMutex keeps existing Lock/Unlock callers compatible while allowing
// request cancellation to abort admission before file-lock or account I/O.
type accountMutex struct{ held chan struct{} }

func (m *accountMutex) Lock()   { _ = m.LockContext(context.Background()) }
func (m *accountMutex) Unlock() { <-m.held }

func (m *accountMutex) LockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.held <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
