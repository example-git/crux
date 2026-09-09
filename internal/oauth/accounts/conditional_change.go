package accounts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/sjson"
)

// ErrStateChanged rejects a conditional operation whose complete private
// account observation no longer matches. It contains no account identity.
var ErrStateChanged = errors.New("account state changed; reload authentication state")

const accountCommitCompletionTimeout = 5 * time.Second

type accountChangeKind uint8

const (
	accountCheck accountChangeKind = iota
	accountSwitch
	accountLogout
	accountRefresh
	accountImport
)

// PendingChange holds the process mutex and captured-path account file lock.
// Call Close immediately via defer after Begin succeeds. Commit retains both
// locks so a caller can finish its local config commit before releasing them.
// Do not call accounts APIs or perform network I/O while holding this lease.
type PendingChange struct {
	state *pendingAccountChange
}

// Every copied public handle refers to the same lifecycle and lock ownership.
type pendingAccountChange struct {
	guard     sync.Mutex
	before    Snapshot
	kind      accountChangeKind
	staged    []byte
	selected  *Entry
	release   func()
	validate  Validator // private refresh owner fence; never mutates account state
	committed bool
	verified  Snapshot // successful fixed postimage; never advanced by verification
	closed    bool
}

func (*pendingAccountChange) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "accounts.pendingAccountChange(private)")
}
func (*pendingAccountChange) MarshalJSON() ([]byte, error) {
	return nil, errors.New("pending account state is private and cannot be serialized")
}

// CommitResult is host-private. Written records a successful account-file
// rename even when post-write capture fails; Snapshot is valid only on success.
type CommitResult struct {
	Snapshot           Snapshot
	Written            bool
	completionDeadline time.Time
}

// CompletionDeadline is the original bounded completion deadline established
// after the account rename. Callers continuing a durable multi-file operation
// must inherit this deadline rather than start another completion window. It is
// zero when Commit did not write, and remains available on post-write errors.
func (result CommitResult) CompletionDeadline() time.Time {
	if !result.Written {
		return time.Time{}
	}
	return result.completionDeadline
}

func (PendingChange) String() string   { return "accounts.PendingChange(private)" }
func (PendingChange) GoString() string { return "accounts.PendingChange(private)" }
func (PendingChange) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "accounts.PendingChange(private)")
}
func (PendingChange) MarshalJSON() ([]byte, error) {
	return nil, errors.New("pending account changes are private and cannot be serialized")
}
func (CommitResult) String() string   { return "accounts.CommitResult(private)" }
func (CommitResult) GoString() string { return "accounts.CommitResult(private)" }
func (CommitResult) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "accounts.CommitResult(private)")
}
func (CommitResult) MarshalJSON() ([]byte, error) {
	return nil, errors.New("account commit results are private and cannot be serialized")
}

// BeginSwitch stages selection of one exact existing account. No file changes
// occur until Commit, including when the requested account is already active.
func (before Snapshot) BeginSwitch(ctx context.Context, namespace, accountID string) (*PendingChange, error) {
	if namespace == "" || accountID == "" || !slices.Contains(before.namespaces, namespace) {
		return nil, errors.New("account switch requires its captured namespace and exact account")
	}
	return before.beginChange(ctx, accountSwitch, namespace, accountID)
}

// BeginLogout stages removal of every account in one exact captured namespace.
// Mutation tombstones remain and selection advances even for an empty namespace.
func (before Snapshot) BeginLogout(ctx context.Context, namespace string) (*PendingChange, error) {
	if namespace == "" || !slices.Contains(before.namespaces, namespace) {
		return nil, errors.New("account logout requires its captured namespace")
	}
	return before.beginChange(ctx, accountLogout, namespace, "")
}

// BeginCheck holds a matching account observation for a config-only operation.
// Its Commit rechecks the observation without writing the account database.
func (before Snapshot) BeginCheck(ctx context.Context) (*PendingChange, error) {
	return before.beginChange(ctx, accountCheck, "", "")
}

func (before Snapshot) beginChange(ctx context.Context, kind accountChangeKind, namespace, accountID string) (*PendingChange, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !before.valid || !filepath.IsAbs(before.path) {
		return nil, errors.New("conditional account change requires a captured snapshot")
	}
	release, err := acquireResolvedLock(ctx, func() (string, error) { return before.path + ".lock", nil })
	if err != nil {
		return nil, privateSnapshotError(err)
	}
	change := &PendingChange{state: &pendingAccountChange{before: before, kind: kind, release: release}}
	ready := false
	defer func() {
		if !ready {
			change.Close()
		}
	}()
	current, err := captureStateAtLocked(ctx, before.path, before.namespaces)
	if err != nil {
		return nil, privateSnapshotError(err)
	}
	if !before.SameObservation(current) {
		return nil, ErrStateChanged
	}
	if kind != accountCheck {
		change.state.staged, change.state.selected, err = stageAccountChange(current.document, kind, namespace, accountID)
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ready = true
	return change, nil
}

// SelectedEntry returns an independent credential copy for a staged switch or import.
// Check/logout leases and closed leases have no selected entry.
func (change *PendingChange) SelectedEntry() (Entry, bool) {
	if change == nil || change.state == nil {
		return Entry{}, false
	}
	state := change.state
	state.guard.Lock()
	defer state.guard.Unlock()
	if state.closed || state.selected == nil {
		return Entry{}, false
	}
	entry := *state.selected
	entry.Raw = bytes.Clone(entry.Raw)
	return entry, true
}

// Commit applies the fixed staged operation once, retaining the lease until
// Close. A failed attempt cannot be retried with the same lease. The caller must
// treat Written=true with an error as account-written/config-pending.
// Cancellation aborts before rename. Once rename succeeds, directory sync and
// verification finish under a separate bounded context; caller cancellation
// cannot undo the durable effect. Blocking filesystem calls are not preemptible.
func (change *PendingChange) Commit(ctx context.Context) (CommitResult, error) {
	if change == nil || change.state == nil {
		return CommitResult{}, errors.New("pending account change is unavailable")
	}
	state := change.state
	state.guard.Lock()
	defer state.guard.Unlock()
	if state.closed || state.committed || state.release == nil {
		return CommitResult{}, errors.New("pending account change is closed or already used")
	}
	state.committed = true
	current, err := state.checkCurrent(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	if state.kind == accountCheck {
		if err := ctx.Err(); err != nil {
			return CommitResult{}, err
		}
		state.verified = current
		return CommitResult{Snapshot: current}, nil
	}
	file, err := os.CreateTemp(filepath.Dir(state.before.path), ".accounts-change-*")
	if err != nil {
		return CommitResult{}, privateSnapshotError(err)
	}
	temporary := file.Name()
	defer func() { _ = file.Close(); _ = os.Remove(temporary) }()
	if err := file.Chmod(0o600); err != nil {
		return CommitResult{}, privateSnapshotError(err)
	}
	if _, err := io.Copy(file, snapshotReader{ctx: ctx, reader: bytes.NewReader(state.staged)}); err != nil {
		return CommitResult{}, privateSnapshotError(err)
	}
	if err := file.Sync(); err != nil {
		return CommitResult{}, privateSnapshotError(err)
	}
	if err := file.Close(); err != nil {
		return CommitResult{}, privateSnapshotError(err)
	}
	// Recheck after staging and any caller config-file wait, immediately before
	// rename. All comparison and I/O use the original captured path.
	if _, err := state.checkCurrent(ctx); err != nil {
		return CommitResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return CommitResult{}, err
	}
	if state.validate != nil {
		if err := state.validate(); err != nil {
			return CommitResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return CommitResult{}, err
	}
	if err := os.Rename(temporary, state.before.path); err != nil {
		return CommitResult{}, privateSnapshotError(err)
	}
	result := CommitResult{Written: true}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), accountCommitCompletionTimeout)
	defer cancel()
	result.completionDeadline, _ = finishCtx.Deadline()
	if err := finishCtx.Err(); err != nil {
		return result, err
	}
	if err := syncAccountDirectory(filepath.Dir(state.before.path)); err != nil {
		return result, privateSnapshotError(err)
	}
	after, err := captureStateAtLocked(finishCtx, state.before.path, state.before.namespaces)
	if err != nil {
		return result, privateSnapshotError(err)
	}
	if !after.file.exists || after.content != sha256.Sum256(state.staged) {
		return result, ErrStateChanged
	}
	if state.validate != nil {
		if err := state.validate(); err != nil {
			return result, err
		}
		// The owner check may take time. Require the captured account file to
		// remain the same through it, rather than retain an already-stale proof.
		current, err := captureStateAtLocked(finishCtx, state.before.path, state.before.namespaces)
		if err != nil {
			return result, privateSnapshotError(err)
		}
		if !after.SameObservation(current) {
			return result, ErrStateChanged
		}
	}
	if err := finishCtx.Err(); err != nil {
		return result, err
	}
	state.verified = after
	result.Snapshot = after
	return result, nil
}

// VerifyCommitted rechecks the exact successful commit observation while its
// original account lease is still held. It does not acquire another account
// lock, take a new path, write, or adopt intervening account changes. BeginCheck
// commits also have a verified observation even though they did not write.
func (change *PendingChange) VerifyCommitted(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if change == nil || change.state == nil {
		return Snapshot{}, errors.New("pending account change is unavailable")
	}
	state := change.state
	state.guard.Lock()
	defer state.guard.Unlock()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if state.closed || state.release == nil || !state.verified.valid {
		return Snapshot{}, errors.New("pending account change has no open verified commit")
	}
	current, err := captureStateAtLocked(ctx, state.verified.path, state.verified.namespaces)
	if err != nil {
		return Snapshot{}, privateSnapshotError(err)
	}
	if !state.verified.SameObservation(current) {
		return Snapshot{}, ErrStateChanged
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return state.verified, nil
}

func (change *pendingAccountChange) checkCurrent(ctx context.Context) (Snapshot, error) {
	current, err := captureStateAtLocked(ctx, change.before.path, change.before.namespaces)
	if err != nil {
		return Snapshot{}, privateSnapshotError(err)
	}
	if !change.before.SameObservation(current) {
		return Snapshot{}, ErrStateChanged
	}
	return current, nil
}

// Close releases the account lease once and never commits or rolls back data.
func (change *PendingChange) Close() {
	if change == nil || change.state == nil {
		return
	}
	state := change.state
	state.guard.Lock()
	defer state.guard.Unlock()
	if state.closed {
		return
	}
	state.closed = true
	if state.release != nil {
		state.release()
		state.release = nil
	}
	state.staged, state.selected = nil, nil
	state.validate = nil
	state.before = Snapshot{}
	state.verified = Snapshot{}
}

func stageAccountChange(document []byte, kind accountChangeKind, namespace, accountID string) ([]byte, *Entry, error) {
	if len(document) == 0 || bytes.Equal(bytes.TrimSpace(document), []byte("null")) {
		document = []byte("{}")
	}
	fields, err := accountObject(document)
	if err != nil {
		return nil, nil, err
	}
	// encoding/json accepts known struct fields without regard to case. Retain
	// their original spelling, rejecting collisions before surgical path edits.
	names := map[string]string{}
	for key := range fields {
		canonical := strings.ToLower(key)
		switch canonical {
		case "active", "accounts", "rotations", "mutations", "selections":
			if _, exists := names[canonical]; exists {
				return nil, nil, errors.New("ambiguous account database fields")
			}
			names[canonical] = key
		}
	}
	objects := make(map[string]map[string]json.RawMessage)
	for _, canonical := range []string{"active", "accounts", "rotations", "mutations", "selections"} {
		name, ok := names[canonical]
		if !ok {
			name = canonical
			names[canonical] = name
		}
		object, err := accountObject(fields[name])
		if err != nil {
			return nil, nil, err
		}
		objects[canonical] = object
	}
	var state store
	if err := json.Unmarshal(document, &state); err != nil {
		return nil, nil, errors.New("invalid account database")
	}
	if state.Active == nil {
		state.Active = map[string]string{}
	}
	if state.Selections == nil {
		state.Selections = map[string]uint64{}
	}
	staged := bytes.Clone(document)
	set := func(parts []string, value any) error {
		var err error
		staged, err = sjson.SetBytes(staged, accountChangePath(parts...), value)
		return err
	}
	del := func(parts ...string) error {
		var err error
		staged, err = sjson.DeleteBytes(staged, accountChangePath(parts...))
		return err
	}
	var selected *Entry
	if kind == accountSwitch {
		for _, entry := range state.Accounts[namespace] {
			if entry.ID == accountID {
				if selected != nil {
					return nil, nil, errors.New("account switch target is not unique")
				}
				copy := entry
				copy.Raw = bytes.Clone(entry.Raw)
				selected = &copy
			}
		}
		if selected == nil {
			return nil, nil, errors.New("account switch target is unavailable")
		}
		if state.Active[namespace] != accountID {
			if state.Selections[namespace] == math.MaxUint64 {
				return nil, nil, errors.New("account selection counter exhausted")
			}
			state.Selections[namespace]++
			if err := set([]string{names["selections"], namespace}, state.Selections[namespace]); err != nil {
				return nil, nil, errors.New("cannot stage account selection")
			}
		}
		state.Active[namespace] = accountID
		if err := set([]string{names["active"], namespace}, accountID); err != nil {
			return nil, nil, errors.New("cannot stage account selection")
		}
	} else {
		if state.Selections[namespace] == math.MaxUint64 {
			return nil, nil, errors.New("account selection counter exhausted")
		}
		if _, err := accountObject(objects["mutations"][namespace]); err != nil {
			return nil, nil, err
		}
		seen := map[string]bool{}
		for _, entry := range state.Accounts[namespace] {
			if entry.ID == "" || seen[entry.ID] {
				return nil, nil, errors.New("account logout contains an ambiguous account identity")
			}
			seen[entry.ID] = true
			if state.Mutations[namespace][entry.ID] == math.MaxUint64 {
				return nil, nil, errors.New("account mutation counter exhausted")
			}
		}
		for id := range seen {
			state.markMutation(namespace, id)
			if err := set([]string{names["mutations"], namespace, id}, state.Mutations[namespace][id]); err != nil {
				return nil, nil, errors.New("cannot stage account removal")
			}
		}
		state.Selections[namespace]++
		if err := set([]string{names["selections"], namespace}, state.Selections[namespace]); err != nil {
			return nil, nil, errors.New("cannot stage account removal")
		}
		for _, field := range []string{"active", "accounts", "rotations"} {
			if err := del(names[field], namespace); err != nil {
				return nil, nil, errors.New("cannot stage account removal")
			}
		}
		delete(state.Active, namespace)
		delete(state.Accounts, namespace)
		delete(state.Rotations, namespace)
	}
	// A typed postimage check ensures path syntax cannot silently select another
	// namespace. Raw unknown fields and untouched entries are never re-encoded.
	var actual store
	if json.Unmarshal(staged, &actual) != nil {
		return nil, nil, errors.New("invalid staged account database")
	}
	if actual.Active == nil {
		actual.Active = map[string]string{}
	}
	if actual.Selections == nil {
		actual.Selections = map[string]uint64{}
	}
	if !reflect.DeepEqual(state, actual) {
		return nil, nil, errors.New("staged account change does not match its exact target")
	}
	return staged, selected, nil
}

// accountObject parses only the object being edited. Nested raw values remain
// untouched; duplicate object keys are rejected to avoid first/last-key drift.
func accountObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]json.RawMessage{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid account database object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid account database object")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate account database field")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errors.New("invalid account database object")
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid account database object")
	}
	return fields, nil
}

func accountChangePath(parts ...string) string {
	escape := strings.NewReplacer("\\", "\\\\", ".", "\\.", ":", "\\:", "|", "\\|", "#", "\\#", "@", "\\@", "*", "\\*", "?", "\\?")
	result := make([]string, len(parts))
	for i, part := range parts {
		result[i] = ":" + escape.Replace(part)
	}
	return strings.Join(result, ".")
}
