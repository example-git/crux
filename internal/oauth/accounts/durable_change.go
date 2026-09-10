package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/oauth"
	"github.com/tidwall/gjson"
)

// DurableChange is a private, fixed account-file change. Its explicit encoding
// is for the owner-side journal, never discovery, receipts or logging.
type (
	DurableChange        struct{ disk durableAccountChange }
	durableAccountChange struct {
		Version    int               `json:"version"`
		Path       string            `json:"path"`
		Namespaces []string          `json:"namespaces"`
		Kind       accountChangeKind `json:"kind"`
		Exists     bool              `json:"exists"`
		Identity   [32]byte          `json:"identity"`
		Size       int64             `json:"size"`
		Mode       os.FileMode       `json:"mode"`
		Modified   int64             `json:"modified"`
		Before     []byte            `json:"before"`
		After      []byte            `json:"after"`
	}
)

func (DurableChange) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[private durable account change]")
}

func (DurableChange) MarshalJSON() ([]byte, error) {
	return nil, errors.New("durable account changes are private")
}
func (d DurableChange) Writes() bool    { return d.disk.Kind != accountCheck }
func (d DurableChange) IsRefresh() bool { return d.disk.Kind == accountRefresh }
func (d DurableChange) Path() string    { return d.disk.Path }
func (d DurableChange) Encode() (json.RawMessage, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(d.disk)
}

func DecodeDurableChange(data json.RawMessage) (DurableChange, error) {
	var d DurableChange
	if !validDurableAccountEnvelope(gjson.ParseBytes(data)) {
		return d, errors.New("ambiguous durable account fields")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d.disk) != nil || decoder.Decode(new(any)) != io.EOF {
		return d, errors.New("invalid durable account change")
	}
	return d, d.validate()
}

func (d DurableChange) validate() error {
	v := d.disk
	if v.Version != 1 || v.Kind > accountRemove || v.Kind == accountCheck && !bytes.Equal(v.Before, v.After) {
		return errors.New("invalid durable account change kind")
	}
	if v.Path == "" {
		if v.Kind != accountCheck || v.Exists || len(v.Namespaces) != 0 || len(v.Before) != 0 || len(v.After) != 0 {
			return errors.New("invalid empty durable account observation")
		}
		return nil
	}
	if !filepath.IsAbs(v.Path) || filepath.Clean(v.Path) != v.Path || !slices.IsSorted(v.Namespaces) || v.Size < 0 || v.Exists && (!v.Mode.IsRegular() || int64(len(v.Before)) != v.Size) || !v.Exists && len(v.Before) != 0 {
		return errors.New("invalid durable account preimage")
	}
	for _, raw := range [][]byte{v.Before, v.After} {
		if len(raw) == 0 {
			continue
		}
		var state store
		if json.Unmarshal(raw, &state) != nil {
			return errors.New("invalid durable account document")
		}
		for _, entries := range state.Accounts {
			for _, entry := range entries {
				registerSecrets(entry)
			}
		}
	}
	if d.Writes() && len(v.After) == 0 {
		return errors.New("durable account postimage is missing")
	}
	return nil
}

func durableAccountState(state *pendingAccountChange) DurableChange {
	before := state.before
	after := state.staged
	if state.kind == accountCheck {
		after = before.document
	}
	return DurableChange{disk: durableAccountChange{Version: 1, Path: before.path, Namespaces: slices.Clone(before.namespaces), Kind: state.kind, Exists: before.file.exists, Identity: before.file.identity, Size: before.file.size, Mode: before.file.mode, Modified: before.file.modified, Before: bytes.Clone(before.document), After: bytes.Clone(after)}}
}

func (change *PendingChange) DurableChange() (DurableChange, error) {
	if change == nil || change.state == nil {
		return DurableChange{}, errors.New("pending account change is unavailable")
	}
	state := change.state
	state.guard.Lock()
	defer state.guard.Unlock()
	if state.closed || state.committed {
		return DurableChange{}, errors.New("pending account change is no longer staged")
	}
	d := durableAccountState(state)
	return d, d.validate()
}

// BeginRepair takes the same captured-path lease as ordinary account writes.
// An already matching postimage is observed only; any new preimage is rejected.
// Callers retain this lease while checking/finishing their paired config edit.
func (d DurableChange) BeginRepair(ctx context.Context) (*PendingChange, bool, error) {
	if err := d.validate(); err != nil {
		return nil, false, err
	}
	v := d.disk
	if v.Path == "" {
		before, err := EmptySnapshot(ctx)
		if err != nil {
			return nil, false, err
		}
		change, err := before.BeginCheck(ctx)
		return change, false, err
	}
	release, err := acquireResolvedLock(ctx, func() (string, error) { return v.Path + ".lock", nil })
	if err != nil {
		return nil, false, privateSnapshotError(err)
	}
	current, err := captureStateAtLocked(ctx, v.Path, v.Namespaces)
	if err != nil {
		release()
		return nil, false, privateSnapshotError(err)
	}
	post := d.Writes() && current.file.exists && bytes.Equal(current.document, v.After)
	expected := accountFileObservation{exists: v.Exists, identity: v.Identity, size: v.Size, mode: v.Mode, modified: v.Modified}
	if !post && (current.file != expected || !bytes.Equal(current.document, v.Before)) {
		release()
		return nil, false, ErrStateChanged
	}
	state := &pendingAccountChange{before: current, kind: v.Kind, staged: bytes.Clone(v.After), release: release}
	if post {
		state.kind, state.staged = accountCheck, nil
	}
	return &PendingChange{state: state}, post, nil
}

type PendingChangeObserver struct {
	Before func(context.Context, DurableChange) error
	After  func(context.Context, DurableChange, bool) error
}
type pendingChangeObserverKey struct{}

// WithPendingChangeObserver installs the admitting config transaction's journal
// boundary. Callbacks must not call account APIs or acquire config write locks.
func WithPendingChangeObserver(ctx context.Context, observer PendingChangeObserver) context.Context {
	return context.WithValue(ctx, pendingChangeObserverKey{}, observer)
}

// DurableObservation freezes an already captured snapshot without taking a new
// lease or reading newer account state.
func (before Snapshot) DurableObservation() (DurableChange, error) {
	if !before.valid {
		return DurableChange{}, errors.New("captured account observation is required")
	}
	d := durableAccountState(&pendingAccountChange{before: before, kind: accountCheck})
	return d, d.validate()
}

// WithObservedRefresh stages an already returned token from the captured
// document. It never calls a refresher or merges newer account-file state.
func (d DurableChange) WithObservedRefresh(namespace, accountID string, token *oauth.Token) (DurableChange, error) {
	if err := d.validate(); err != nil {
		return DurableChange{}, err
	}
	if d.disk.Kind != accountCheck || token == nil || token.AccessToken == "" || token.ExpiresAt > math.MaxInt64/1000 || !slices.Contains(d.disk.Namespaces, namespace) {
		return DurableChange{}, errors.New("captured refresh result is unavailable")
	}
	target, err := readInactiveRefreshTarget(d.disk.Before, namespace, accountID)
	if err != nil {
		return DurableChange{}, err
	}
	fresh := FromToken(target.entry.ID, target.entry.DisplayName, token, &target.entry)
	registerSecrets(fresh)
	staged, err := stageInactiveRefresh(d.disk.Before, namespace, accountID, target, fresh)
	if err != nil {
		return DurableChange{}, err
	}
	d.disk.Kind = accountRefresh
	d.disk.After = staged
	return d, d.validate()
}

func validDurableAccountEnvelope(v gjson.Result) bool {
	if !v.IsObject() {
		return false
	}
	seen := map[string]bool{}
	valid := true
	v.ForEach(func(key, value gjson.Result) bool {
		name := strings.ToLower(key.Str)
		if seen[name] {
			valid = false
			return false
		}
		seen[name] = true
		return true
	})
	return valid
}
