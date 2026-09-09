package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/example-git/crux/internal/lock"
	"github.com/tidwall/gjson"
)

const (
	AuthenticationJournalOAuth          = "oauth-exchange"
	AuthenticationJournalLocal          = "local-change"
	AuthenticationJournalClient         = "client-publication"
	maxAuthenticationJournalRecords     = 128
	maxAuthenticationJournalBytes       = 256 << 20
	maxAuthenticationJournalRecordBytes = 2*MaxRemoteRuntimeBytes + 1<<20

	// Unresolved records retain room for an explicit terminal marker even when
	// their caller has consumed its reservation. The largest current marker is
	// a review-abandon request: one bounded public owner/target (<11 KiB), a
	// <=4 KiB account choice, fixed-size action IDs and envelope fields. Even
	// sixfold JSON string escaping fits below 128 KiB. OAuth/local retirement
	// adds only fixed flags/revisions. This allowance is internal accounting,
	// not part of ReservedBytes(), and is released only by a terminal record.
	authenticationJournalTransitionBytes = 128 << 10
)

// AuthenticationJournalKey names a private owner-side operation record. Its
// payload is never a public authentication DTO, event, or receiver input.
type AuthenticationJournalKey struct {
	Kind        string `json:"kind"`
	WorkspaceID string `json:"workspace_id"`
	OperationID string `json:"operation_id"`
}

type authenticationOperationContextKey struct{}

// Only the admitting owner service attaches this exact operation identity.
// Transaction code chooses and validates the matching finite record kind.
func ContextWithAuthenticationOperation(ctx context.Context, key AuthenticationJournalKey) context.Context {
	return context.WithValue(ctx, authenticationOperationContextKey{}, key)
}

func AuthenticationOperationFromContext(ctx context.Context) (AuthenticationJournalKey, bool) {
	key, found := ctx.Value(authenticationOperationContextKey{}).(AuthenticationJournalKey)
	return key, found
}

func (key AuthenticationJournalKey) Validate() error { return key.validate() }

func (key AuthenticationJournalKey) validate() error {
	if key.Kind != AuthenticationJournalOAuth && key.Kind != AuthenticationJournalLocal && key.Kind != AuthenticationJournalClient {
		return errors.New("unsupported authentication journal record kind")
	}
	for _, value := range []string{key.WorkspaceID, key.OperationID} {
		if value == "" || len(value) > 512 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid authentication journal identity")
		}
	}
	return nil
}

func (key AuthenticationJournalKey) id() string {
	encoded, _ := json.Marshal(key)
	return selectedTokenBytesID(encoded)
}

type AuthenticationJournal struct {
	store *ConfigStore
	path  string
	scope string
}

func (AuthenticationJournal) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication journals are private")
}
func (AuthenticationJournal) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication journal]"))
}

type AuthenticationJournalEntry struct {
	key       AuthenticationJournalKey
	revision  uint64
	completed bool
	reserved  int
	payload   json.RawMessage
}

func (entry AuthenticationJournalEntry) Revision() uint64         { return entry.revision }
func (entry AuthenticationJournalEntry) Payload() json.RawMessage { return bytes.Clone(entry.payload) }
func (entry AuthenticationJournalEntry) Completed() bool          { return entry.completed }
func (entry AuthenticationJournalEntry) ReservedBytes() int       { return entry.reserved }
func (AuthenticationJournalEntry) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication journal entries are private")
}
func (AuthenticationJournalEntry) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication journal entry]"))
}

type authenticationJournalDisk struct {
	Version  int                                    `json:"version"`
	Sequence uint64                                 `json:"sequence"`
	Records  map[string]authenticationJournalRecord `json:"records"`
}
type authenticationJournalRecord struct {
	Key       AuthenticationJournalKey `json:"key"`
	Revision  uint64                   `json:"revision"`
	Completed bool                     `json:"completed,omitempty"`
	Reserved  int                      `json:"reserved_bytes,omitempty"`
	Payload   json.RawMessage          `json:"payload"`
}

func authenticationJournalReservation(completed bool, reserved int) int {
	if !completed {
		return reserved + authenticationJournalTransitionBytes
	}
	return reserved
}

func (authenticationJournalDisk) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication journal file]"))
}
func (authenticationJournalRecord) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication journal record]"))
}

// CaptureAuthenticationJournal captures only the owning store's fixed path.
// It performs no disk I/O, and later calls never consult the ambient home.
func (s *ConfigStore) CaptureAuthenticationJournal(ctx context.Context) (AuthenticationJournal, error) {
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return AuthenticationJournal{}, err
	}
	defer s.writeMu.RUnlock()
	return s.captureAuthenticationJournalLocked()
}

// captureAuthenticationJournalLocked is for mutation code already holding
// writeMu. Re-entering its read lock can deadlock when a writer is pending.
func (s *ConfigStore) captureAuthenticationJournalLocked() (AuthenticationJournal, error) {
	if err := s.RuntimeRevocation(); err != nil {
		return AuthenticationJournal{}, err
	}
	s.configMu.RLock()
	clientOwned := s.clientRuntime != nil
	s.configMu.RUnlock()
	if clientOwned {
		return AuthenticationJournal{}, ErrClientRuntimeManaged
	}
	if !filepath.IsAbs(s.globalDataPath) {
		return AuthenticationJournal{}, errors.New("authentication journal requires its captured owning configuration path")
	}
	scope, _ := json.Marshal([]string{s.globalDataPath, s.workspacePath, s.workingDir})
	return AuthenticationJournal{store: s, path: filepath.Clean(s.globalDataPath) + ".authentication-journal.json", scope: selectedTokenBytesID(scope)}, nil
}

func (journal AuthenticationJournal) locked(ctx context.Context) (context.Context, func(), error) {
	if journal.store == nil || !filepath.IsAbs(journal.path) {
		return ctx, nil, errors.New("authentication journal is unavailable")
	}
	bound, cancel := journal.store.BindRuntimeContext(ctx)
	if err := journal.store.RuntimeRevocation(); err != nil {
		cancel()
		return bound, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(journal.path), 0700); err != nil {
		cancel()
		return bound, nil, authenticationInputError(err)
	}
	limited, stop := context.WithTimeout(bound, configLockDeadline)
	release, err := lock.File(limited, journal.path+".lock")
	stop()
	if err != nil {
		cancel()
		return bound, nil, authenticationInputError(err)
	}
	return bound, func() { release(); cancel() }, nil
}

func (journal AuthenticationJournal) Load(ctx context.Context, key AuthenticationJournalKey) (AuthenticationJournalEntry, bool, error) {
	if err := key.validate(); err != nil {
		return AuthenticationJournalEntry{}, false, err
	}
	ctx, release, err := journal.locked(ctx)
	if err != nil {
		return AuthenticationJournalEntry{}, false, err
	}
	defer release()
	_, data, err := readAuthenticationJournal(ctx, journal.path)
	if err != nil {
		return AuthenticationJournalEntry{}, false, err
	}
	record, found := data.Records[key.id()]
	if !found {
		return AuthenticationJournalEntry{}, false, nil
	}
	if record.Key != key {
		return AuthenticationJournalEntry{}, false, errors.New("authentication journal identity conflict")
	}
	return AuthenticationJournalEntry{key: key, revision: record.Revision, completed: record.Completed, reserved: record.Reserved, payload: bytes.Clone(record.Payload)}, true, nil
}

// ScopeID binds captured owning configuration paths without exporting them.
func (journal AuthenticationJournal) ScopeID() string { return journal.scope }

// AllKeys lists private identities across workspace incarnations. Typed callers
// must still match connection, principal and captured path scope before exposing
// even redacted history. It grants no publication or mutation authority.
func (journal AuthenticationJournal) AllKeys(ctx context.Context, kind string) ([]AuthenticationJournalKey, error) {
	return journal.keys(ctx, kind, "", true)
}

// Keys returns identities only. Typed recovery callers must load and validate
// each private record; this listing alone establishes no operation outcome.
func (journal AuthenticationJournal) Keys(ctx context.Context, kind, workspaceID string) ([]AuthenticationJournalKey, error) {
	return journal.keys(ctx, kind, workspaceID, false)
}
func (journal AuthenticationJournal) keys(ctx context.Context, kind, workspaceID string, all bool) ([]AuthenticationJournalKey, error) {
	validationWorkspace := workspaceID
	if all {
		validationWorkspace = "history"
	}
	if err := (AuthenticationJournalKey{Kind: kind, WorkspaceID: validationWorkspace, OperationID: "listing"}).validate(); err != nil {
		return nil, err
	}
	ctx, release, err := journal.locked(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	_, data, err := readAuthenticationJournal(ctx, journal.path)
	if err != nil {
		return nil, err
	}
	var keys []AuthenticationJournalKey
	for _, record := range data.Records {
		if record.Key.Kind == kind && (all || record.Key.WorkspaceID == workspaceID) {
			keys = append(keys, record.Key)
		}
	}
	return keys, nil
}

// Store uses a durable compare-and-swap revision. A returned write failure is
// ambiguous; callers must Load and compare their exact intended bytes before
// proceeding. An unresolved record is never pruned to admit another operation.
func (journal AuthenticationJournal) Store(ctx context.Context, key AuthenticationJournalKey, expected uint64, payload json.RawMessage, completed bool, reservation ...int) (AuthenticationJournalEntry, error) {
	reserved := 0
	if len(reservation) > 1 {
		return AuthenticationJournalEntry{}, errors.New("authentication journal has ambiguous capacity reservation")
	}
	if len(reservation) == 1 {
		reserved = reservation[0]
	}
	if reserved < 0 || reserved > maxAuthenticationJournalRecordBytes || completed && reserved != 0 {
		return AuthenticationJournalEntry{}, errors.New("authentication journal has invalid capacity reservation")
	}
	if err := key.validate(); err != nil {
		return AuthenticationJournalEntry{}, err
	}
	if len(payload)+authenticationJournalReservation(completed, reserved) > maxAuthenticationJournalRecordBytes || !authenticationLayerObject(payload) {
		return AuthenticationJournalEntry{}, errors.New("authentication journal payload is invalid or exceeds its limit")
	}
	ctx, release, err := journal.locked(ctx)
	if err != nil {
		return AuthenticationJournalEntry{}, err
	}
	defer release()
	before, data, err := readAuthenticationJournal(ctx, journal.path)
	if err != nil {
		return AuthenticationJournalEntry{}, err
	}
	previous, exists := data.Records[key.id()]
	if exists && (previous.Key != key || previous.Revision != expected) || !exists && expected != 0 {
		return AuthenticationJournalEntry{}, errors.New("authentication journal revision changed")
	}
	if exists && previous.Completed && (!completed || !bytes.Equal(previous.Payload, payload)) {
		return AuthenticationJournalEntry{}, errors.New("completed authentication journal record cannot be changed")
	}
	if exists && bytes.Equal(previous.Payload, payload) && previous.Completed == completed && previous.Reserved == reserved {
		return AuthenticationJournalEntry{key: key, revision: previous.Revision, completed: completed, reserved: reserved, payload: bytes.Clone(payload)}, nil
	}
	if data.Sequence == ^uint64(0) {
		return AuthenticationJournalEntry{}, errors.New("authentication journal sequence exhausted")
	}
	data.Records = maps.Clone(data.Records)
	data.Sequence++
	data.Records[key.id()] = authenticationJournalRecord{Key: key, Revision: data.Sequence, Completed: completed, Reserved: reserved, Payload: bytes.Clone(payload)}
	encoded, err := json.Marshal(data)
	if err != nil {
		return AuthenticationJournalEntry{}, errors.New("authentication journal payload cannot be encoded")
	}
	capacity := len(encoded)
	for _, record := range data.Records {
		capacity += authenticationJournalReservation(record.Completed, record.Reserved)
	}
	pruned := false
	for len(data.Records) > maxAuthenticationJournalRecords || capacity > maxAuthenticationJournalBytes {
		oldest := ""
		for id, record := range data.Records {
			if id != key.id() && record.Completed && (oldest == "" || record.Revision < data.Records[oldest].Revision) {
				oldest = id
			}
		}
		if oldest == "" {
			return AuthenticationJournalEntry{}, errors.New("authentication journal capacity is reserved by unresolved operations")
		}
		// Count this exact compact JSON member and its separating comma. The
		// new/updated record remains in the map, so deletion never empties it.
		// Marshal the full file only once more after pruning, rather than once
		// per old result when a large new reservation needs several evictions.
		removed := data.Records[oldest]
		member, err := json.Marshal(map[string]authenticationJournalRecord{oldest: removed})
		if err != nil {
			return AuthenticationJournalEntry{}, errors.New("authentication journal record cannot be encoded")
		}
		capacity -= len(member) - 1 + authenticationJournalReservation(removed.Completed, removed.Reserved)
		delete(data.Records, oldest)
		pruned = true
	}
	if pruned {
		encoded, err = json.Marshal(data)
		capacity = len(encoded)
		for _, record := range data.Records {
			capacity += authenticationJournalReservation(record.Completed, record.Reserved)
		}
	}
	if err != nil || capacity > maxAuthenticationJournalBytes {
		return AuthenticationJournalEntry{}, errors.New("authentication journal storage limit reached")
	}
	stage, err := stageAuthenticationScopeWrite(ctx, before, authenticationCredentialEdit{path: journal.path, data: encoded})
	if err != nil {
		return AuthenticationJournalEntry{}, err
	}
	defer stage.Close()
	if _, _, err := stage.Commit(ctx); err != nil {
		return AuthenticationJournalEntry{}, err
	}
	return AuthenticationJournalEntry{key: key, revision: data.Sequence, completed: completed, reserved: reserved, payload: bytes.Clone(payload)}, nil
}

func readAuthenticationJournal(ctx context.Context, path string) (authenticationInputFile, authenticationJournalDisk, error) {
	before := authenticationInputFile{path: path}
	data := authenticationJournalDisk{Version: 1, Records: map[string]authenticationJournalRecord{}}
	file, err := openAuthenticationInput(path)
	if errors.Is(err, os.ErrNotExist) {
		return before, data, ctx.Err()
	}
	if err != nil {
		return before, data, authenticationInputError(err)
	}
	defer file.Close()
	before.info, err = observeAuthenticationInput(file)
	if err != nil || before.info.size > maxAuthenticationJournalBytes || before.info.mode.Perm()&0077 != 0 {
		return before, data, errors.New("authentication journal has invalid size or permissions")
	}
	before.data, err = io.ReadAll(io.LimitReader(authenticationInputReader{ctx: ctx, reader: file}, maxAuthenticationJournalBytes+1))
	if err != nil || len(before.data) > maxAuthenticationJournalBytes {
		return before, data, errors.New("authentication journal cannot be read within its limit")
	}
	if err := verifyAuthenticationInput(ctx, path, file, before.info); err != nil {
		return before, data, authenticationInputError(err)
	}
	if !authenticationJournalSurface(gjson.ParseBytes(before.data)) {
		return before, data, errors.New("authentication journal has ambiguous fields")
	}
	decoder := json.NewDecoder(bytes.NewReader(before.data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&data) != nil || decoder.Decode(new(any)) != io.EOF || data.Version != 1 || data.Records == nil || len(data.Records) > maxAuthenticationJournalRecords {
		return before, data, errors.New("authentication journal is malformed or unsupported")
	}
	// Admission and reads enforce the same implicit transition room. Reject
	// oversubscribed files visibly; do not rewrite them or evict unresolved
	// operations to manufacture capacity during a read.
	capacity := len(before.data)
	for id, record := range data.Records {
		if record.Key.validate() != nil || id != record.Key.id() || record.Revision == 0 || record.Revision > data.Sequence || record.Reserved < 0 || record.Reserved > maxAuthenticationJournalRecordBytes || record.Completed && record.Reserved != 0 || len(record.Payload)+authenticationJournalReservation(record.Completed, record.Reserved) > maxAuthenticationJournalRecordBytes || !authenticationLayerObject(record.Payload) {
			return before, data, errors.New("authentication journal record is invalid")
		}
		capacity += authenticationJournalReservation(record.Completed, record.Reserved)
		if capacity > maxAuthenticationJournalBytes {
			return before, data, errors.New("authentication journal capacity is oversubscribed")
		}
	}
	if capacity > maxAuthenticationJournalBytes {
		return before, data, errors.New("authentication journal capacity is oversubscribed")
	}
	return before, data, ctx.Err()
}

// Payload belongs to its typed caller. Configuration maps inside it may
// legitimately use case-distinct keys; only the journal envelope is checked here.
func authenticationJournalSurface(value gjson.Result) bool {
	if !value.IsObject() {
		return false
	}
	seen := map[string]bool{}
	valid := true
	value.ForEach(func(key, child gjson.Result) bool {
		name := strings.ToLower(key.Str)
		if seen[name] {
			valid = false
			return false
		}
		seen[name] = true
		if name != "payload" && child.IsObject() {
			valid = authenticationJournalSurface(child)
		}
		return valid
	})
	return valid
}
