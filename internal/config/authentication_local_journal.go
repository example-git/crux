package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/tidwall/gjson"
)

var ErrLocalAuthenticationRefreshUnresolved = errors.New("refresh exchange may have consumed its token without a retained response; sign in again")

var ErrLocalAuthenticationRecoveryRequired = errors.New("authentication operation is retained on disk; review its original operation and repair saved state explicitly")

// LocalAuthenticationProgress reports observations made by the original writer.
// Repair progress is separate and cannot manufacture its old runtime publication.
type LocalAuthenticationProgress struct {
	AccountRefreshed bool `json:"account_refreshed"`
	AccountsSaved    bool `json:"accounts_saved"`
	ConfigSaved      bool `json:"config_saved"`
	RuntimePublished bool `json:"runtime_published"`
}
type LocalAuthenticationSummary struct {
	WorkspaceID      string                      `json:"workspace_id"`
	OperationID      string                      `json:"operation_id"`
	Revision         uint64                      `json:"revision"`
	Action           string                      `json:"action"`
	ProviderID       string                      `json:"provider_id"`
	AccountID        string                      `json:"account_id,omitempty"`
	RemovedAccountID string                      `json:"removed_account_id,omitempty"`
	Original         LocalAuthenticationProgress `json:"original"`
	RefreshStarted   bool                        `json:"refresh_started"`
	RefreshObserved  bool                        `json:"refresh_observed"`
	Finished         bool                        `json:"finished"`
	Coherent         bool                        `json:"coherent"`
	RepairReady      bool                        `json:"repair_ready"`
	NeedsReload      bool                        `json:"needs_reload"`
}
type LocalAuthenticationRepairResult struct {
	Summary         LocalAuthenticationSummary `json:"summary"`
	AccountsWritten bool                       `json:"accounts_written"`
	AccountsMatched bool                       `json:"accounts_matched"`
	ConfigWritten   bool                       `json:"config_written"`
	ConfigMatched   bool                       `json:"config_matched"`
	NeedsReload     bool                       `json:"needs_reload"`
}

type localAuthenticationFile struct {
	Path     string      `json:"path"`
	Data     []byte      `json:"data"`
	Exists   bool        `json:"exists"`
	Identity [32]byte    `json:"identity"`
	Size     int64       `json:"size"`
	Mode     os.FileMode `json:"mode"`
	Modified int64       `json:"modified"`
}

func localAuthenticationFileFrom(f authenticationInputFile) localAuthenticationFile {
	return localAuthenticationFile{f.path, bytes.Clone(f.data), f.info.exists, f.info.identity, f.info.size, f.info.mode, f.info.modified}
}
func (f localAuthenticationFile) input() authenticationInputFile {
	return authenticationInputFile{path: f.Path, data: bytes.Clone(f.Data), info: authenticationInputFileInfo{exists: f.Exists, identity: f.Identity, size: f.Size, mode: f.Mode, modified: f.Modified}}
}

type localAuthenticationDisk struct {
	Version            int                                `json:"version"`
	Key                AuthenticationJournalKey           `json:"key"`
	Action             string                             `json:"action"`
	Owner              providerregistry.RegistrationOwner `json:"owner"`
	AccountID          string                             `json:"account_id,omitempty"`
	RemoveID           string                             `json:"remove_id,omitempty"`
	GlobalPath         string                             `json:"global_path"`
	WorkspacePath      string                             `json:"workspace_path"`
	WorkingDir         string                             `json:"working_dir"`
	ScopePath          string                             `json:"scope_path,omitempty"`
	Inputs             []localAuthenticationFile          `json:"inputs"`
	BeforeOrder        []string                           `json:"before_order"`
	Order              []string                           `json:"order"`
	WrittenPaths       []string                           `json:"written_paths,omitempty"`
	ConfigAfter        []byte                             `json:"config_after,omitempty"`
	InitialAccounts    json.RawMessage                    `json:"initial_accounts"`
	Accounts           json.RawMessage                    `json:"accounts,omitempty"`
	RefreshAccounts    json.RawMessage                    `json:"refresh_accounts,omitempty"`
	CredentialEffectID string                             `json:"credential_effect_id,omitempty"`
	RefreshStarted     bool                               `json:"refresh_started"`
	RefreshToken       *oauth.Token                       `json:"refresh_token,omitempty"`
	Original           LocalAuthenticationProgress        `json:"original"`
	Finished           bool                               `json:"finished"`
	Coherent           bool                               `json:"coherent"`
	RepairBase         uint64                             `json:"repair_base"`
	RepairStarted      bool                               `json:"repair_started"`
	Repair             LocalAuthenticationRepairResult    `json:"repair"`
}
type LocalAuthenticationChange struct {
	journal  AuthenticationJournal
	revision uint64
	disk     localAuthenticationDisk
}

func (LocalAuthenticationChange) MarshalJSON() ([]byte, error) {
	return nil, errors.New("local authentication repair captures are private")
}
func (LocalAuthenticationChange) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "[private local authentication repair capture]")
}
func (d localAuthenticationDisk) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "[private local authentication change]")
}
func (c LocalAuthenticationChange) Summary() LocalAuthenticationSummary {
	d := c.disk
	return LocalAuthenticationSummary{WorkspaceID: d.Key.WorkspaceID, OperationID: d.Key.OperationID, Revision: c.revision, Action: d.Action, ProviderID: d.Owner.ProviderID, AccountID: d.AccountID, RemovedAccountID: d.RemoveID, Original: d.Original, RefreshStarted: d.RefreshStarted, RefreshObserved: d.RefreshToken != nil, Finished: d.Finished, Coherent: d.Coherent, RepairReady: len(d.Accounts) > 0 || len(d.RefreshAccounts) > 0 || d.RefreshToken != nil, NeedsReload: d.Repair.NeedsReload}
}
func (s *ConfigStore) LoadAuthenticationLocalChange(ctx context.Context, key AuthenticationJournalKey) (LocalAuthenticationChange, bool, error) {
	journal, err := s.CaptureAuthenticationJournal(ctx)
	if err != nil {
		return LocalAuthenticationChange{}, false, err
	}
	return loadAuthenticationLocalChange(ctx, journal, key)
}
func loadAuthenticationLocalChange(ctx context.Context, journal AuthenticationJournal, key AuthenticationJournalKey) (LocalAuthenticationChange, bool, error) {
	if key.Kind != AuthenticationJournalLocal {
		return LocalAuthenticationChange{}, false, errors.New("local authentication operation is required")
	}
	entry, found, err := journal.Load(ctx, key)
	if err != nil || !found {
		return LocalAuthenticationChange{}, found, err
	}
	var d localAuthenticationDisk
	raw := entry.Payload()
	if !localAuthenticationSurface(gjson.ParseBytes(raw)) {
		return LocalAuthenticationChange{}, false, errors.New("local authentication record has ambiguous fields")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d) != nil || decoder.Decode(new(any)) != io.EOF || d.Version != 1 || d.Key != key || d.Owner.ProviderID == "" {
		return LocalAuthenticationChange{}, false, errors.New("local authentication record is malformed")
	}
	switch d.Action {
	case "switch", "logout", "remove", "oauth-login", "api-key":
	default:
		return LocalAuthenticationChange{}, false, errors.New("local authentication action is unsupported")
	}
	for _, path := range []string{d.GlobalPath, d.WorkspacePath, d.WorkingDir, d.ScopePath} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path) {
			return LocalAuthenticationChange{}, false, errors.New("local authentication path is invalid")
		}
	}
	if d.GlobalPath == "" || journal.path != d.GlobalPath+".authentication-journal.json" {
		return LocalAuthenticationChange{}, false, errors.New("local authentication owner path changed")
	}
	seen := map[string]bool{}
	for _, file := range d.Inputs {
		if !filepath.IsAbs(file.Path) || filepath.Clean(file.Path) != file.Path || seen[file.Path] || file.Size < 0 || file.Exists && (!file.Mode.IsRegular() || int64(len(file.Data)) != file.Size) || !file.Exists && len(file.Data) != 0 {
			return LocalAuthenticationChange{}, false, errors.New("local authentication input is invalid")
		}
		seen[file.Path] = true
	}
	for _, path := range append(append(slices.Clone(d.Order), d.BeforeOrder...), d.WrittenPaths...) {
		if !seen[path] {
			return LocalAuthenticationChange{}, false, errors.New("local authentication topology is invalid")
		}
	}
	if d.ScopePath != "" && d.ScopePath != d.GlobalPath && d.ScopePath != d.WorkspacePath {
		return LocalAuthenticationChange{}, false, errors.New("local authentication writable scope changed")
	}
	if len(d.ConfigAfter) > 0 && (!seen[d.ScopePath] || isShellConfig(d.ScopePath) || !authenticationLayerObject(d.ConfigAfter) || !slices.Contains(d.WrittenPaths, d.ScopePath)) {
		return LocalAuthenticationChange{}, false, errors.New("local authentication config postimage is invalid")
	}
	var accountPath string
	for _, raw := range []json.RawMessage{d.InitialAccounts, d.Accounts, d.RefreshAccounts} {
		if len(raw) == 0 {
			continue
		}
		change, err := accounts.DecodeDurableChange(raw)
		if err != nil {
			return LocalAuthenticationChange{}, false, err
		}
		if accountPath != "" && change.Path() != accountPath {
			return LocalAuthenticationChange{}, false, errors.New("local authentication account path changed")
		}
		accountPath = change.Path()
	}
	if d.Original.RuntimePublished && !d.Original.ConfigSaved || d.Coherent && (!d.Finished || !d.Original.RuntimePublished && !(d.Action == "remove" && d.Original.AccountsSaved)) || d.Repair.NeedsReload && (!d.RepairStarted || d.RepairBase == 0) {
		return LocalAuthenticationChange{}, false, errors.New("local authentication progress is inconsistent")
	}
	if d.CredentialEffectID != "" && (d.Action != "api-key" || len(d.CredentialEffectID) != 64 || strings.Trim(d.CredentialEffectID, "0123456789abcdef") != "") {
		return LocalAuthenticationChange{}, false, errors.New("local authentication effect identity is invalid")
	}
	if len(d.InitialAccounts) == 0 || d.RefreshToken != nil && !d.RefreshStarted {
		return LocalAuthenticationChange{}, false, errors.New("local authentication refresh state is invalid")
	}
	if d.RefreshToken != nil {
		registerOAuthTokenSecrets(d.RefreshToken)
	}
	// Only credential-bearing configuration values are registered; public owner
	// identifiers and operation IDs remain visible for exact recovery.
	for _, file := range d.Inputs {
		registerLocalAuthenticationConfigSecrets(file.Data)
	}
	registerLocalAuthenticationConfigSecrets(d.ConfigAfter)
	return LocalAuthenticationChange{journal: journal, revision: entry.Revision(), disk: d}, true, nil
}
func registerLocalAuthenticationConfigSecrets(raw []byte) {
	var c Config
	if json.Unmarshal(raw, &c) == nil {
		registerConfigSecrets(&c)
	}
}

type localAuthenticationWriter struct{ capture LocalAuthenticationChange }

func (s *ConfigStore) beginLocalAuthenticationChangeLocked(ctx context.Context, before AuthenticationCapture, admitted authenticationAdmission, owner providerregistry.RegistrationOwner, action, accountID, removeID string) (context.Context, *localAuthenticationWriter, error) {
	raw := ctx.Value(authenticationOperationContextKey{})
	if raw == nil {
		return ctx, nil, nil
	}
	key, ok := raw.(AuthenticationJournalKey)
	if !ok || key.validate() != nil {
		return ctx, nil, errors.New("invalid authentication operation context")
	}
	key.Kind = AuthenticationJournalLocal
	journal, err := s.captureAuthenticationJournalLocked()
	if err != nil {
		return ctx, nil, err
	}
	if _, found, err := journal.Load(ctx, key); err != nil {
		return ctx, nil, err
	} else if found {
		return ctx, nil, ErrLocalAuthenticationRecoveryRequired
	}
	initial, err := before.accounts.DurableObservation()
	if err != nil {
		return ctx, nil, err
	}
	initialRaw, err := initial.Encode()
	if err != nil {
		return ctx, nil, err
	}
	d := localAuthenticationDisk{Version: 1, Key: key, Action: action, Owner: owner, AccountID: accountID, RemoveID: removeID, GlobalPath: s.globalDataPath, WorkspacePath: s.workspacePath, WorkingDir: s.workingDir, ScopePath: admitted.path, BeforeOrder: slices.Clone(before.inputs.order), Order: slices.Clone(before.inputs.order), InitialAccounts: initialRaw}
	for _, file := range before.inputs.files {
		d.Inputs = append(d.Inputs, localAuthenticationFileFrom(file))
	}
	writer := &localAuthenticationWriter{capture: LocalAuthenticationChange{journal: journal, disk: d}}
	if err := writer.save(ctx); err != nil {
		return ctx, nil, err
	}
	ctx = accounts.WithPendingChangeObserver(ctx, accounts.PendingChangeObserver{Before: writer.beforeAccounts, After: writer.afterAccounts})
	return ctx, writer, nil
}
func (w *localAuthenticationWriter) save(ctx context.Context) error {
	if w == nil {
		return nil
	}
	raw, err := json.Marshal(w.capture.disk)
	if err != nil {
		return errors.New("local authentication record cannot be encoded")
	}
	entry, err := w.capture.journal.Store(ctx, w.capture.disk.Key, w.capture.revision, raw, w.capture.disk.Coherent || w.capture.disk.Repair.NeedsReload, 2<<20)
	if err != nil {
		// A rename followed by an observation error is ambiguous. Continue only if
		// reloading proves that this exact payload is the retained successor.
		check, found, loadErr := w.capture.journal.Load(ctx, w.capture.disk.Key)
		if loadErr != nil || !found || check.Revision() <= w.capture.revision || !bytes.Equal(check.Payload(), raw) {
			return errors.Join(err, loadErr)
		}
		entry = check
	}
	w.capture.revision = entry.Revision()
	return nil
}
func (w *localAuthenticationWriter) beforeAccounts(ctx context.Context, change accounts.DurableChange) error {
	if w == nil {
		return nil
	}
	raw, err := change.Encode()
	if err != nil {
		return err
	}
	if change.IsRefresh() {
		w.capture.disk.RefreshAccounts = raw
	} else {
		w.capture.disk.Accounts = raw
	}
	return w.save(ctx)
}
func (w *localAuthenticationWriter) afterAccounts(ctx context.Context, change accounts.DurableChange, written bool) error {
	if w == nil || !written {
		return nil
	}
	if change.IsRefresh() {
		w.capture.disk.Original.AccountRefreshed = true
	} else {
		w.capture.disk.Original.AccountsSaved = true
	}
	return w.save(ctx)
}
func (w *localAuthenticationWriter) stage(ctx context.Context, stage *authenticationScopeWrite, topology authenticationScopeTopology, pending *accounts.PendingChange) error {
	if w == nil {
		return nil
	}
	d := &w.capture.disk
	accountChange, err := pending.DurableChange()
	if err != nil {
		return err
	}
	accountRaw, err := accountChange.Encode()
	if err != nil {
		return err
	}
	d.Accounts = accountRaw
	d.ScopePath = stage.before.path
	d.ConfigAfter = bytes.Clone(stage.data)
	d.Order = slices.Clone(topology.inputs.order)
	d.WrittenPaths = slices.Clone(topology.writtenPaths)
	d.Inputs = nil
	for _, file := range topology.inputs.files {
		d.Inputs = append(d.Inputs, localAuthenticationFileFrom(file))
	}
	return w.save(ctx)
}
func (w *localAuthenticationWriter) configSaved(ctx context.Context, written bool, deadline time.Time) error {
	if w == nil || !written {
		return nil
	}
	w.capture.disk.Original.ConfigSaved = true
	finish, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	return w.save(finish)
}
func (w *localAuthenticationWriter) finish(ctx context.Context, result AuthenticationMutationResult, failure *error) {
	if w == nil {
		return
	}
	d := &w.capture.disk
	d.Original.AccountRefreshed = d.Original.AccountRefreshed || result.AccountRefreshed
	d.Original.AccountsSaved = d.Original.AccountsSaved || result.AccountsSaved
	d.Original.ConfigSaved = d.Original.ConfigSaved || result.ConfigSaved
	d.Original.RuntimePublished = d.Original.RuntimePublished || result.RuntimePublished
	d.Finished = true
	_, coherent := result.RuntimeSnapshot()
	d.Coherent = *failure == nil && coherent
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), authenticationCompletionTimeout)
	defer cancel()
	*failure = errors.Join(*failure, w.save(finish))
}
func (w *localAuthenticationWriter) refresher(next accounts.Refresher) accounts.Refresher {
	if w == nil || next == nil {
		return next
	}
	return func(ctx context.Context, refresh string) (*oauth.Token, error) {
		if w.capture.disk.RefreshStarted {
			return nil, ErrLocalAuthenticationRecoveryRequired
		}
		w.capture.disk.RefreshStarted = true
		if err := w.save(ctx); err != nil {
			return nil, err
		}
		token, err := next(ctx, refresh)
		if token == nil || err != nil {
			return nil, err
		}
		frozen := *token
		if token.Client != nil {
			client := *token.Client
			frozen.Client = &client
		}
		registerOAuthTokenSecrets(&frozen)
		w.capture.disk.RefreshToken = &frozen
		if saveErr := w.save(ctx); saveErr != nil {
			return nil, errors.Join(err, saveErr)
		}
		return &frozen, err
	}
}

// OriginalConfiguredCredentialEffectID is private intent evidence, not an old
// runtime or proof that disk repair has run.
func (c LocalAuthenticationChange) OriginalConfiguredCredentialEffectID() (string, bool) {
	return c.disk.CredentialEffectID, c.disk.CredentialEffectID != ""
}

func localAuthenticationSurface(value gjson.Result) bool {
	valid := true
	if value.IsArray() {
		value.ForEach(func(_, child gjson.Result) bool { valid = localAuthenticationSurface(child); return valid })
		return valid
	}
	if !value.IsObject() {
		return true
	}
	seen := map[string]bool{}
	value.ForEach(func(key, child gjson.Result) bool {
		name := strings.ToLower(key.Str)
		if seen[name] {
			valid = false
			return false
		}
		seen[name] = true
		valid = localAuthenticationSurface(child)
		return valid
	})
	return valid
}
func (s LocalAuthenticationSummary) Validate() error {
	key := AuthenticationJournalKey{Kind: AuthenticationJournalLocal, WorkspaceID: s.WorkspaceID, OperationID: s.OperationID}
	if key.Validate() != nil || s.Revision == 0 {
		return errors.New("local authentication summary identity is invalid")
	}
	for _, value := range []string{s.ProviderID, s.AccountID, s.RemovedAccountID} {
		if len(value) > 4096 || !utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return errors.New("local authentication summary field is invalid")
		}
	}
	if s.ProviderID == "" || s.RefreshObserved && !s.RefreshStarted || s.Original.RuntimePublished && !s.Original.ConfigSaved {
		return errors.New("local authentication summary progress is invalid")
	}
	switch s.Action {
	case "switch", "logout", "remove", "oauth-login", "api-key":
	default:
		return errors.New("local authentication summary action is invalid")
	}
	return nil
}
