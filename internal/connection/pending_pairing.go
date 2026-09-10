package connection

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/redact"
)

const (
	pendingPairingLimit = 64
	pendingPairingBytes = 4 << 20
)

// PendingPairing contains only public recovery information. Pending private
// identities never appear in listing, errors, or authorization request bodies.
type PendingPairing struct {
	OperationID       string
	Name              string
	PromotionName     string
	Address           string
	ClientFingerprint string
	ServerFingerprint string
	CreatedAt         time.Time
}

type pendingPairing struct {
	OperationID   string     `json:"operation_id"`
	CreatedAt     int64      `json:"created_at"`
	PromotionName string     `json:"promotion_name"`
	Connection    Connection `json:"connection"`
}

func (pendingPairing) String() string   { return "[private pending pairing]" }
func (pendingPairing) GoString() string { return "[private pending pairing]" }

type pendingPairings struct {
	Version int                       `json:"version"`
	Entries map[string]pendingPairing `json:"entries"`
}

type pendingImage struct {
	content []byte
	info    os.FileInfo
}

func pendingPairingPath(path string) string { return path + ".pending-pairing.json" }

// PairingPendingError preserves the exact operation after any ambiguous remote
// outcome or local publication failure. Recovery never generates a new key.
type PairingPendingError struct {
	OperationID string
	Cause       error
}

func (e *PairingPendingError) Error() string {
	return fmt.Sprintf("pairing remains pending; recover the same identity with `crux connections recover %s`: %v", e.OperationID, e.Cause)
}
func (e *PairingPendingError) Unwrap() error { return e.Cause }

type PairingSavedError struct {
	Name  string
	Cause error
}

func (e *PairingSavedError) Error() string {
	return fmt.Sprintf("connection %q was saved; pending cleanup could not be confirmed: %v", e.Name, e.Cause)
}
func (e *PairingSavedError) Unwrap() error { return e.Cause }

func validPairingName(name string) bool {
	return name != "" && name == strings.TrimSpace(name) && len(name) <= 128 && strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func validatePending(p pendingPairing) error {
	id, err := base64.RawURLEncoding.DecodeString(p.OperationID)
	if err != nil || len(id) != 32 || base64.RawURLEncoding.EncodeToString(id) != p.OperationID || p.CreatedAt <= 0 || !validPairingName(p.Connection.Name) {
		return errors.New("invalid pending pairing identity")
	}
	if p.PromotionName != "" && !validPairingName(p.PromotionName) {
		return errors.New("invalid pending pairing promotion name")
	}
	if len(p.Connection.Client.PrivateKey) > 16<<10 {
		return errors.New("pending pairing private identity exceeds its bound")
	}
	address, err := NormalizeConnectionAddress(p.Connection.Address)
	if err != nil || address != p.Connection.Address {
		return errors.New("invalid pending pairing address")
	}
	if _, err := parseHistoricalCertificate(p.Connection.ServerCertificate, x509.ExtKeyUsageServerAuth); err != nil {
		return errors.New("invalid pending pairing server identity")
	}
	client, err := parseHistoricalCertificate(p.Connection.Client.Certificate, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return errors.New("invalid pending pairing client identity")
	}
	if _, _, err := parseIdentityKey(p.Connection.Client, client); err != nil {
		return errors.New("invalid pending pairing client identity")
	}
	redact.Register(p.Connection.Client.PrivateKey)
	return nil
}

// readPendingPairings is called under the connection-store lock. It rejects
// symlinks, permissive files, duplicate/case-aliased fields and changing files.
func readPendingPairings(path string) (pendingPairings, pendingImage, error) {
	data := pendingPairings{Version: 1, Entries: make(map[string]pendingPairing)}
	image := pendingImage{}
	path = pendingPairingPath(path)
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return data, image, nil
	}
	if err != nil || !before.Mode().IsRegular() || before.Size() > pendingPairingBytes {
		return data, image, errors.New("pending pairing store is not a bounded private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return data, image, fmt.Errorf("open pending pairing store: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return data, image, errors.New("pending pairing store changed while opening")
	}
	if err := fsext.ValidatePrivateFile(file); err != nil {
		return data, image, errors.New("pending pairing store is not private")
	}
	content, err := io.ReadAll(io.LimitReader(file, pendingPairingBytes+1))
	if err != nil || len(content) > pendingPairingBytes {
		return data, image, errors.New("read bounded pending pairing store")
	}
	after, err := os.Lstat(path)
	if err != nil || !samePendingFile(before, after) {
		return data, image, errors.New("pending pairing store changed while reading")
	}
	if err := validatePendingJSON(content); err != nil {
		return data, image, err
	}
	if err := json.Unmarshal(content, &data); err != nil || data.Version != 1 || data.Entries == nil || len(data.Entries) > pendingPairingLimit {
		return data, image, errors.New("invalid pending pairing store")
	}
	names := make(map[string]bool)
	for id, entry := range data.Entries {
		if id != entry.OperationID || names[entry.Connection.Name] || (entry.PromotionName != "" && entry.PromotionName != entry.Connection.Name && names[entry.PromotionName]) || validatePending(entry) != nil {
			return data, image, errors.New("invalid or repeated pending pairing entry")
		}
		names[entry.Connection.Name] = true
		if entry.PromotionName != "" {
			names[entry.PromotionName] = true
		}
	}
	return data, pendingImage{content: content, info: after}, nil
}

func samePendingFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && b.Mode().IsRegular() && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func writePendingPairings(path string, data pendingPairings, before pendingImage) error {
	content, err := json.Marshal(data)
	if err != nil || len(content) > pendingPairingBytes || len(data.Entries) > pendingPairingLimit {
		return errors.New("pending pairing store capacity exceeded")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".pending-pairing-*.json")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	_, current, err := readPendingPairings(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(before.content, current.content) || (before.info == nil) != (current.info == nil) || (before.info != nil && !samePendingFile(before.info, current.info)) {
		return errors.New("pending pairing store changed before publication")
	}
	if err := replaceStoreFile(temporary, pendingPairingPath(path)); err != nil {
		return err
	}
	if err := syncPairingDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	_, published, err := readPendingPairings(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(content, published.content) {
		return errors.New("pending pairing store publication was replaced")
	}
	return nil
}

// Windows uses a single write-through replacement. Directory fsync is not
// available there; this is process-restart recovery, not a power-loss claim.
func syncPairingDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func stagePendingPairing(ctx context.Context, path string, created Connection) (pendingPairing, error) {
	if _, err := parseCertificate(created.ServerCertificate, x509.ExtKeyUsageServerAuth); err != nil {
		return pendingPairing{}, err
	}
	if _, _, err := parseIdentity(created.Client, x509.ExtKeyUsageClientAuth); err != nil {
		return pendingPairing{}, err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return pendingPairing{}, err
	}
	entry := pendingPairing{OperationID: base64.RawURLEncoding.EncodeToString(raw[:]), CreatedAt: time.Now().Unix(), Connection: created}
	if err := validatePending(entry); err != nil {
		return pendingPairing{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return pendingPairing{}, err
	}
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return pendingPairing{}, err
	}
	defer release()
	data, err := readStoreAt(path)
	if err != nil {
		return pendingPairing{}, err
	}
	if _, exists := data.Connections[created.Name]; exists {
		return pendingPairing{}, fmt.Errorf("connection already exists: %s", created.Name)
	}
	pending, before, err := readPendingPairings(path)
	if err != nil {
		return pendingPairing{}, err
	}
	if len(pending.Entries) >= pendingPairingLimit {
		return pendingPairing{}, errors.New("pending pairing capacity reached; recover or explicitly forget an existing operation")
	}
	for _, existing := range pending.Entries {
		if pendingReservesName(existing, created.Name) {
			return pendingPairing{}, &PairingPendingError{OperationID: existing.OperationID, Cause: errors.New("connection name is reserved by a pending identity")}
		}
	}
	if _, exists := pending.Entries[entry.OperationID]; exists {
		return pendingPairing{}, errors.New("pending pairing operation collision")
	}
	if err := ctx.Err(); err != nil {
		return pendingPairing{}, err
	}
	pending.Entries[entry.OperationID] = entry
	if err := writePendingPairings(path, pending, before); err != nil {
		current, _, readErr := readPendingPairings(path)
		if readErr == nil && current.Entries[entry.OperationID] == entry {
			return entry, &PairingPendingError{OperationID: entry.OperationID, Cause: fmt.Errorf("authorization was not sent because durable key persistence was not confirmed: %w", err)}
		}
		return pendingPairing{}, fmt.Errorf("authorization was not sent; pending key persistence failed (inspect `crux connections pending` before retrying): %w", err)
	}
	return entry, nil
}

func ListPendingPairings(ctx context.Context) ([]PendingPairing, error) {
	path, err := filepath.Abs(storePath())
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return nil, err
	}
	defer release()
	pending, _, err := readPendingPairings(path)
	if err != nil {
		return nil, err
	}
	result := make([]PendingPairing, 0, len(pending.Entries))
	for _, entry := range pending.Entries {
		client, _ := parseHistoricalCertificate(entry.Connection.Client.Certificate, x509.ExtKeyUsageClientAuth)
		server, _ := parseHistoricalCertificate(entry.Connection.ServerCertificate, x509.ExtKeyUsageServerAuth)
		result = append(result, PendingPairing{OperationID: entry.OperationID, Name: entry.Connection.Name, PromotionName: entry.PromotionName, Address: entry.Connection.Address, ClientFingerprint: certificateFingerprint(client), ServerFingerprint: certificateFingerprint(server), CreatedAt: time.Unix(entry.CreatedAt, 0).UTC()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OperationID < result[j].OperationID })
	return result, nil
}

func RecoverPairing(ctx context.Context, operationID, alternateName string) (Connection, error) {
	path, err := filepath.Abs(storePath())
	if err != nil {
		return Connection{}, err
	}
	entry, err := pendingPairingAt(ctx, path, operationID)
	if err != nil {
		return Connection{}, err
	}
	if alternateName != "" && !validPairingName(alternateName) {
		return Connection{}, errors.New("invalid recovery connection name")
	}
	if _, err := ConfirmAuthorization(ctx, entry.Connection); err != nil {
		return Connection{}, &PairingPendingError{OperationID: operationID, Cause: fmt.Errorf("pinned server did not prove authorization for the retained client identity: %w", err)}
	}
	return promotePendingPairing(ctx, path, entry, alternateName)
}

func pendingPairingAt(ctx context.Context, path, id string) (pendingPairing, error) {
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return pendingPairing{}, err
	}
	defer release()
	pending, _, err := readPendingPairings(path)
	if err != nil {
		return pendingPairing{}, err
	}
	entry, exists := pending.Entries[id]
	if !exists {
		return pendingPairing{}, errors.New("pending pairing operation not found")
	}
	return entry, nil
}

func promotePendingPairing(ctx context.Context, path string, entry pendingPairing, alternateName string) (Connection, error) {
	fail := func(err error) (Connection, error) {
		return Connection{}, &PairingPendingError{OperationID: entry.OperationID, Cause: err}
	}
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return fail(err)
	}
	defer release()
	pending, before, err := readPendingPairings(path)
	if err != nil {
		return fail(err)
	}
	if current, ok := pending.Entries[entry.OperationID]; !ok || current != entry {
		return fail(errors.New("pending pairing identity changed before publication"))
	}
	created := entry.Connection
	if _, err := parseCertificate(created.ServerCertificate, x509.ExtKeyUsageServerAuth); err != nil {
		return fail(err)
	}
	if _, _, err := parseIdentity(created.Client, x509.ExtKeyUsageClientAuth); err != nil {
		return fail(err)
	}
	if entry.PromotionName != "" {
		created.Name = entry.PromotionName
	}
	data, err := readStoreAt(path)
	if err != nil {
		return fail(err)
	}
	if entry.PromotionName != "" && alternateName != "" && alternateName != entry.PromotionName {
		if existing, ok := data.Connections[entry.PromotionName]; ok && existing == created {
			return fail(fmt.Errorf("connection is already saved as %q; recover without an alternate name to complete cleanup", entry.PromotionName))
		}
	}
	if alternateName != "" {
		created.Name = alternateName
	}
	for id, other := range pending.Entries {
		if id != entry.OperationID && pendingReservesName(other, created.Name) {
			return fail(errors.New("recovery name is reserved by another pending identity"))
		}
	}
	if existing, ok := data.Connections[created.Name]; ok && existing != created {
		return fail(errors.New("local connection name conflicts; recover with an explicit unused alternate name"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if entry.PromotionName != created.Name {
		entry.PromotionName = created.Name
		pending.Entries[entry.OperationID] = entry
		if err := writePendingPairings(path, pending, before); err != nil {
			return fail(err)
		}
		pending, before, err = readPendingPairings(path)
		if err != nil {
			return fail(err)
		}
	}
	if existing, ok := data.Connections[created.Name]; !ok || existing != created {
		data.Connections[created.Name] = created
		if err := writeStoreAt(path, data, nil); err != nil {
			return fail(err)
		}
		if err := syncPairingDirectory(filepath.Dir(path)); err != nil {
			return fail(err)
		}
	}
	published, err := readStoreAt(path)
	if err != nil || published.Connections[created.Name] != created {
		return fail(errors.New("could not confirm saved connection; pending identity retained"))
	}
	delete(pending.Entries, entry.OperationID)
	if err := writePendingPairings(path, pending, before); err != nil {
		return created, &PairingSavedError{Name: created.Name, Cause: err}
	}
	return created, nil
}

// ForgetPendingPairing is an explicit local key deletion, never an implicit
// response to timeout/denial/expiry. It does not revoke a server-side grant.
func ForgetPendingPairing(ctx context.Context, operationID string) error {
	path, err := filepath.Abs(storePath())
	if err != nil {
		return err
	}
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer release()
	pending, before, err := readPendingPairings(path)
	if err != nil {
		return err
	}
	if _, ok := pending.Entries[operationID]; !ok {
		return errors.New("pending pairing operation not found")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(pending.Entries, operationID)
	return writePendingPairings(path, pending, before)
}

func validatePendingJSON(content []byte) error {
	bad := errors.New("invalid pending pairing JSON fields")
	d := json.NewDecoder(bytes.NewReader(content))
	d.UseNumber()
	var value func(int) (any, error)
	value = func(depth int) (any, error) {
		if depth > 6 {
			return nil, bad
		}
		t, err := d.Token()
		if err != nil {
			return nil, bad
		}
		if delimiter, ok := t.(json.Delim); ok {
			if delimiter != '{' {
				return nil, bad
			}
			m := map[string]any{}
			for d.More() {
				t, err := d.Token()
				key, ok := t.(string)
				if err != nil || !ok {
					return nil, bad
				}
				if _, exists := m[key]; exists {
					return nil, bad
				}
				v, err := value(depth + 1)
				if err != nil {
					return nil, err
				}
				m[key] = v
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return nil, bad
			}
			return m, nil
		}
		return t, nil
	}
	v, err := value(0)
	if err != nil {
		return bad
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return bad
	}
	fields := func(v any, keys ...string) (map[string]any, bool) {
		m, ok := v.(map[string]any)
		if !ok || len(m) != len(keys) {
			return nil, false
		}
		for _, key := range keys {
			if _, ok := m[key]; !ok {
				return nil, false
			}
		}
		return m, true
	}
	root, ok := fields(v, "version", "entries")
	if !ok {
		return bad
	}
	entries, ok := root["entries"].(map[string]any)
	if !ok || len(entries) > pendingPairingLimit {
		return bad
	}
	for _, v := range entries {
		entry, ok := fields(v, "operation_id", "created_at", "promotion_name", "connection")
		if !ok {
			return bad
		}
		if _, ok := entry["promotion_name"].(string); !ok {
			return bad
		}
		conn, ok := fields(entry["connection"], "name", "address", "server_certificate", "client")
		if !ok {
			return bad
		}
		if _, ok := fields(conn["client"], "certificate", "private_key"); !ok {
			return bad
		}
	}
	return nil
}

func pendingReservesName(entry pendingPairing, name string) bool {
	return entry.Connection.Name == name || entry.PromotionName == name
}

func pairingNameAvailable(ctx context.Context, path, name string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer release()
	data, err := readStoreAt(path)
	if err != nil {
		return err
	}
	if _, exists := data.Connections[name]; exists {
		return fmt.Errorf("connection already exists: %s", name)
	}
	pending, _, err := readPendingPairings(path)
	if err != nil {
		return err
	}
	for _, entry := range pending.Entries {
		if pendingReservesName(entry, name) {
			return &PairingPendingError{OperationID: entry.OperationID, Cause: errors.New("connection name is reserved by a pending identity")}
		}
	}
	return nil
}
