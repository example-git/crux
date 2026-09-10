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
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/tidwall/sjson"
)

// InactiveRefreshResult is private credential material. Snapshot is an
// authorized successor only on success. Written remains true if a token was
// saved but unrelated state changed or subsequent observation failed.
type InactiveRefreshResult struct {
	Entry    Entry
	Snapshot Snapshot
	Written  bool
}

func (InactiveRefreshResult) String() string   { return "accounts.InactiveRefreshResult(private)" }
func (InactiveRefreshResult) GoString() string { return "accounts.InactiveRefreshResult(private)" }
func (InactiveRefreshResult) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "accounts.InactiveRefreshResult(private)")
}

func (InactiveRefreshResult) MarshalJSON() ([]byte, error) {
	return nil, errors.New("inactive account refresh results are private and cannot be serialized")
}

// RefreshInactive refreshes one captured account without changing selection.
// The exact owner validator must be side-effect-free and must not acquire
// config write locks or call account APIs. The supplied refresher is the only
// exchange authority; no global registered refresher is consulted. Call this
// outside config/account locks. Stale pre-exchange observations are rejected,
// including peer rotations; they are never exchanged again or blindly adopted.
func (before Snapshot) RefreshInactive(ctx context.Context, namespace, accountID string, refresher Refresher, validate Validator) (InactiveRefreshResult, error) {
	if err := ctx.Err(); err != nil {
		return InactiveRefreshResult{}, err
	}
	if !before.valid || !filepath.IsAbs(before.path) || namespace == "" || accountID == "" || !slices.Contains(before.namespaces, namespace) || validate == nil {
		return InactiveRefreshResult{}, errors.New("inactive refresh requires its captured account and owner validator")
	}
	if err := validate(); err != nil {
		return InactiveRefreshResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return InactiveRefreshResult{}, err
	}
	path := inactiveRefreshLockPath(before.path, namespace, accountID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return InactiveRefreshResult{}, privateSnapshotError(err)
	}
	release, err := lock.File(ctx, path)
	if err != nil {
		return InactiveRefreshResult{}, privateSnapshotError(err)
	}
	defer release()
	initial, err := before.BeginCheck(ctx)
	if err != nil {
		return InactiveRefreshResult{}, err
	}
	defer initial.Close()
	if err := validate(); err != nil {
		return InactiveRefreshResult{}, err
	}
	target, err := readInactiveRefreshTarget(before.document, namespace, accountID)
	if err != nil {
		return InactiveRefreshResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return InactiveRefreshResult{}, err
	}
	if target.entry.RefreshToken == "" || target.entry.AccessToken != "" && !target.entry.Expired() {
		checked, err := initial.Commit(ctx)
		if err != nil {
			return InactiveRefreshResult{}, err
		}
		if err := validate(); err != nil {
			return InactiveRefreshResult{}, err
		}
		if err := ctx.Err(); err != nil {
			return InactiveRefreshResult{}, err
		}
		return InactiveRefreshResult{Entry: cloneSnapshotEntries([]Entry{target.entry})[0], Snapshot: checked.Snapshot}, nil
	}
	if refresher == nil {
		return InactiveRefreshResult{}, errors.New("account has no exact refresh capability; sign in again")
	}
	initial.Close()
	if err := ctx.Err(); err != nil {
		return InactiveRefreshResult{}, err
	}
	if err := validate(); err != nil {
		return InactiveRefreshResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return InactiveRefreshResult{}, err
	}
	// Once the exchange may consume a refresh token, preserve a valid returned
	// rotation through caller disconnects, conditional on exact target/owner.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	exchangeCtx := providertransport.ContextWithOwnerValidator(commitCtx, providertransport.OwnerValidator(validate))
	token, err := refresher(exchangeCtx, target.entry.RefreshToken)
	if err != nil {
		return InactiveRefreshResult{}, err
	}
	if token == nil || token.AccessToken == "" {
		return InactiveRefreshResult{}, errors.New("account refresh returned no access token")
	}
	if token.ExpiresAt > math.MaxInt64/1000 {
		return InactiveRefreshResult{}, errors.New("account refresh expiry exceeds storage range")
	}
	fresh := FromToken(target.entry.ID, target.entry.DisplayName, token, &target.entry)
	fresh.Raw = bytes.Clone(fresh.Raw)
	registerSecrets(fresh)
	if err := validate(); err != nil {
		return InactiveRefreshResult{}, err
	}
	result, err := before.saveInactiveRefresh(commitCtx, namespace, accountID, target, fresh, validate)
	if stopped := ctx.Err(); stopped != nil {
		// Saving a consumed rotation is still necessary, but a disconnected
		// caller is not authorized to continue into account activation.
		result.Snapshot = Snapshot{}
		return result, errors.Join(err, stopped)
	}
	return result, err
}

func inactiveRefreshLockPath(database, namespace, accountID string) string {
	digest := sha256.Sum256([]byte(namespace + "\x00" + accountID))
	return filepath.Join(filepath.Dir(database), "locks", fmt.Sprintf("%x.account-refresh.lock", digest))
}

func (before Snapshot) saveInactiveRefresh(ctx context.Context, namespace, accountID string, target inactiveRefreshTarget, fresh Entry, validate Validator) (InactiveRefreshResult, error) {
	release, err := acquireResolvedLock(ctx, func() (string, error) { return before.path + ".lock", nil })
	if err != nil {
		return InactiveRefreshResult{}, privateSnapshotError(err)
	}
	change := &PendingChange{state: &pendingAccountChange{kind: accountRefresh, release: release, validate: validate}}
	defer change.Close()
	if err := validate(); err != nil {
		return InactiveRefreshResult{}, err
	}
	current, err := captureStateAtLocked(ctx, before.path, before.namespaces)
	if err != nil {
		return InactiveRefreshResult{}, privateSnapshotError(err)
	}
	authorized := before.SameObservation(current)
	latest, err := readInactiveRefreshTarget(current.document, namespace, accountID)
	if err != nil {
		return InactiveRefreshResult{}, ErrStateChanged
	}
	if !target.sameTarget(latest) || !authorized && inactiveRefreshJSONEqual(before.document, current.document) {
		return InactiveRefreshResult{}, ErrStateChanged
	}
	staged, err := stageInactiveRefresh(current.document, namespace, accountID, latest, fresh)
	if err != nil {
		return InactiveRefreshResult{}, err
	}
	change.state.before, change.state.staged = current, staged
	committed, err := change.Commit(ctx)
	result := InactiveRefreshResult{Written: committed.Written}
	if committed.Written {
		result.Entry = cloneSnapshotEntries([]Entry{fresh})[0]
	}
	if err != nil {
		return result, err
	}
	if !authorized {
		return result, ErrStateChanged
	}
	result.Snapshot = committed.Snapshot
	return result, nil
}

// The complete raw target and counter/history fields fence manual edits,
// same-value Save, metadata number spellings, absent/null and foreign fields.
// The comparisons are stronger than the legacy CredentialID rotation key.
type inactiveRefreshTarget struct {
	entry      Entry
	index      int
	rootNames  map[string]string
	entryNames map[string]string
	raw        json.RawMessage
	mutation   json.RawMessage
	history    json.RawMessage
}

func (target inactiveRefreshTarget) sameTarget(other inactiveRefreshTarget) bool {
	return inactiveRefreshRawEqual(target.raw, other.raw) &&
		inactiveRefreshRawEqual(target.mutation, other.mutation) &&
		inactiveRefreshRawEqual(target.history, other.history)
}

func (inactiveRefreshTarget) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "accounts.inactiveRefreshTarget(private)")
}

func (inactiveRefreshTarget) MarshalJSON() ([]byte, error) {
	return nil, errors.New("inactive refresh targets are private and cannot be serialized")
}

func inactiveRefreshRawEqual(left, right []byte) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	var a, b bytes.Buffer
	return json.Compact(&a, left) == nil && json.Compact(&b, right) == nil && bytes.Equal(a.Bytes(), b.Bytes())
}

func inactiveRefreshJSONEqual(left, right []byte) bool {
	if len(left) == 0 || len(right) == 0 {
		return len(left) == 0 && len(right) == 0
	}
	decode := func(data []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		err := decoder.Decode(&value)
		if err == nil && decoder.Decode(new(any)) != io.EOF {
			err = errors.New("invalid private account value")
		}
		return value, err
	}
	a, aerr := decode(left)
	b, berr := decode(right)
	return aerr == nil && berr == nil && reflect.DeepEqual(a, b)
}

func inactiveRefreshFieldNames(object map[string]json.RawMessage, known ...string) (map[string]string, error) {
	names := map[string]string{}
	for key := range object {
		for _, canonical := range known {
			if strings.EqualFold(key, canonical) {
				if _, exists := names[canonical]; exists {
					return nil, errors.New("ambiguous account database fields")
				}
				names[canonical] = key
			}
		}
	}
	for _, key := range known {
		if _, exists := names[key]; !exists {
			names[key] = key
		}
	}
	return names, nil
}

func readInactiveRefreshTarget(document []byte, namespace, accountID string) (inactiveRefreshTarget, error) {
	fields, err := accountObject(document)
	if err != nil {
		return inactiveRefreshTarget{}, err
	}
	names, err := inactiveRefreshFieldNames(fields, "active", "accounts", "mutations", "rotations", "selections")
	if err != nil {
		return inactiveRefreshTarget{}, err
	}
	objects := map[string]map[string]json.RawMessage{}
	for key, name := range names {
		object, err := accountObject(fields[name])
		if err != nil {
			return inactiveRefreshTarget{}, err
		}
		objects[key] = object
	}
	var entries []json.RawMessage
	if json.Unmarshal(objects["accounts"][namespace], &entries) != nil {
		return inactiveRefreshTarget{}, errors.New("inactive refresh account is unavailable")
	}
	target := inactiveRefreshTarget{index: -1, rootNames: names}
	for index, raw := range entries {
		object, err := accountObject(raw)
		if err != nil {
			return inactiveRefreshTarget{}, err
		}
		entryNames, err := inactiveRefreshFieldNames(object, "id", "displayName", "accessToken", "refreshToken", "expiresAt", "raw")
		if err != nil {
			return inactiveRefreshTarget{}, err
		}
		var entry Entry
		if json.Unmarshal(raw, &entry) != nil {
			return inactiveRefreshTarget{}, errors.New("invalid inactive refresh account")
		}
		if entry.ID == accountID {
			if target.index != -1 {
				return inactiveRefreshTarget{}, errors.New("inactive refresh account is not unique")
			}
			target.entry, target.index, target.entryNames, target.raw = entry, index, entryNames, raw
		}
	}
	if target.index == -1 {
		return inactiveRefreshTarget{}, errors.New("inactive refresh account is unavailable")
	}
	for _, key := range []string{"mutations", "rotations"} {
		object, err := accountObject(objects[key][namespace])
		if err != nil {
			return inactiveRefreshTarget{}, err
		}
		if key == "mutations" {
			target.mutation = object[accountID]
		} else {
			target.history = object[accountID]
		}
	}
	return target, nil
}

func stageInactiveRefresh(document []byte, namespace, accountID string, target inactiveRefreshTarget, fresh Entry) ([]byte, error) {
	staged := bytes.Clone(document)
	prefix := accountChangePath(target.rootNames["accounts"], namespace) + fmt.Sprintf(".%d.", target.index)
	for _, field := range []struct {
		name  string
		value any
	}{{"accessToken", fresh.AccessToken}, {"refreshToken", fresh.RefreshToken}, {"expiresAt", fresh.ExpiresAt}} {
		var err error
		staged, err = sjson.SetBytes(staged, prefix+accountChangePath(target.entryNames[field.name]), field.value)
		if err != nil {
			return nil, errors.New("cannot stage inactive account refresh")
		}
	}
	var history []json.RawMessage
	if len(target.history) > 0 && json.Unmarshal(target.history, &history) != nil {
		return nil, errors.New("invalid account rotation history")
	}
	link, _ := json.Marshal(rotation{Before: CredentialID(target.entry), After: CredentialID(fresh)})
	history = append(history, link)
	if len(history) > 8 {
		history = history[len(history)-8:]
	}
	rawHistory, err := json.Marshal(history)
	if err != nil {
		return nil, errors.New("cannot encode account rotation history")
	}
	staged, err = sjson.SetRawBytes(staged, accountChangePath(target.rootNames["rotations"], namespace, accountID), rawHistory)
	if err != nil {
		return nil, errors.New("cannot stage account rotation history")
	}
	var expected, actual store
	if json.Unmarshal(document, &expected) != nil || json.Unmarshal(staged, &actual) != nil {
		return nil, errors.New("invalid inactive refresh account database")
	}
	expected.Accounts[namespace][target.index] = fresh
	if expected.Rotations == nil {
		expected.Rotations = map[string]map[string][]rotation{}
	}
	if expected.Rotations[namespace] == nil {
		expected.Rotations[namespace] = map[string][]rotation{}
	}
	var typedHistory []rotation
	if json.Unmarshal(rawHistory, &typedHistory) != nil {
		return nil, errors.New("invalid staged account rotation history")
	}
	expected.Rotations[namespace][accountID] = typedHistory
	if !reflect.DeepEqual(expected, actual) {
		return nil, errors.New("staged refresh does not match its exact account")
	}
	return staged, nil
}
