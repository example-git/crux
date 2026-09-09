package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/tidwall/gjson"
)

// No verifier, authorization code, or device polling secret is persisted. A
// record can recover a known token result, never repeat an uncertain exchange.
type oauthLoginJournalRecord struct {
	Version   int                                `json:"version"`
	Scope     string                             `json:"scope,omitempty"`
	Owner     providerregistry.RegistrationOwner `json:"owner"`
	Capture   string                             `json:"capture"`
	Started   bool                               `json:"started"`
	Abandoned bool                               `json:"abandoned,omitempty"`
	Token     *oauth.Token                       `json:"token,omitempty"`
}

func (oauthLoginJournalRecord) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth login journal result]"))
}

type oauthLoginJournal struct {
	mu        sync.Mutex
	journal   AuthenticationJournal
	key       AuthenticationJournalKey
	revision  uint64
	record    oauthLoginJournalRecord
	completed bool
}

func (oauthLoginJournal) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth login journal handle]"))
}

func oauthLoginCaptureID(p *oauthLoginPreparation) (string, error) {
	if p == nil || !p.before.inputs.valid || p.before.runtime.config == nil {
		return "", errors.New("OAuth recovery capture is unavailable")
	}
	inputs := make([]selectedTokenInputProof, 0, len(p.before.inputs.files))
	for _, file := range p.before.inputs.files {
		inputs = append(inputs, selectedTokenProof(file))
	}
	type accountSelection struct {
		Active  string
		Entries []accounts.Entry
	}
	selected := map[string]accountSelection{}
	for _, owner := range p.before.owners {
		if owner.AccountNamespace != "" {
			selected[owner.AccountNamespace] = accountSelection{Active: p.before.accounts.ActiveID(owner.AccountNamespace), Entries: p.before.accounts.Entries(owner.AccountNamespace)}
		}
	}
	type inputBasis struct {
		Path, Raw, Evaluated string
		Exists               bool
	}
	var basis []inputBasis
	if captured := p.before.runtime.config.authenticationBasis; captured != nil && captured.valid {
		for _, path := range captured.order {
			source, found := captured.sources[path]
			if !found {
				return "", errAuthenticationBasisUnavailable
			}
			basis = append(basis, inputBasis{path, selectedTokenBytesID(source.raw), selectedTokenBytesID(source.evaluated), source.exists})
		}
	} else {
		return "", errAuthenticationBasisUnavailable
	}
	bundle := ""
	if p.owner.HasManifest {
		scan := p.before.runtime.config.providerScan
		if scan == nil {
			return "", errors.New("OAuth recovery bundle capture is unavailable")
		}
		status, exists := scan.pluginStatuses[p.owner.ManifestID]
		if !exists {
			return "", errors.New("OAuth recovery bundle owner is unavailable")
		}
		bundle = status.Digest
	}
	// Hash every captured input, including evaluated source output and complete
	// account metadata. None of these bytes become public recovery metadata.
	data, err := json.Marshal([]any{p.owner, p.registration.Manifest, bundle, p.before.runtime.config, selectedTokenEnvironmentID(p.before.runtime.Environment()), p.before.inputs.order, inputs, basis, selected, p.settings.globalPath, p.settings.workspacePath, p.settings.workingDir, p.settings.overrides, p.before.runtime.config.authenticationBasis.noUnset})
	if err != nil {
		return "", errors.New("OAuth recovery capture cannot be encoded")
	}
	return selectedTokenBytesID(data), nil
}

func decodeOAuthLoginJournal(entry AuthenticationJournalEntry) (oauthLoginJournalRecord, error) {
	var record oauthLoginJournalRecord
	if !selectedTokenLineageUnambiguous(gjson.ParseBytes(entry.Payload())) {
		return record, errors.New("OAuth recovery record has ambiguous fields")
	}
	decoder := json.NewDecoder(bytes.NewReader(entry.Payload()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Version != 1 || record.Owner.ProviderID == "" || len(record.Capture) != 64 || record.Token != nil && !record.Started || record.Abandoned && (record.Token != nil || !entry.Completed()) {
		return record, errors.New("OAuth recovery record is invalid")
	}
	if record.Token != nil {
		registerOAuthTokenSecrets(record.Token)
		if err := validateRemoteOAuthToken(record.Token); err != nil {
			return record, errors.New("OAuth recovery token result is invalid")
		}
	}
	return record, nil
}

func (s *ConfigStore) prepareOAuthLoginJournal(ctx context.Context, p *oauthLoginPreparation) error {
	key, found := AuthenticationOperationFromContext(ctx)
	if !found {
		return nil
	} // Legacy in-process callers have no service operation.
	if err := key.Validate(); err != nil {
		return err
	}
	if key.Kind != AuthenticationJournalOAuth {
		return errors.New("OAuth login operation has the wrong journal kind")
	}
	journal, err := s.CaptureAuthenticationJournal(ctx)
	if err != nil {
		return err
	}
	captureID, err := oauthLoginCaptureID(p)
	if err != nil {
		return err
	}
	entry, exists, err := journal.Load(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		record, err := decodeOAuthLoginJournal(entry)
		if err != nil {
			return err
		}
		if record.Scope != journal.ScopeID() || record.Owner != p.owner || record.Capture != captureID {
			return errors.New("OAuth operation conflicts with its durable captured intent")
		}
		// Recovery is a separately selected action. A reused Begin ID cannot
		// turn a historical token or unknown exchange into a new authorization.
		return errors.New("OAuth operation already has a durable record; recover its observed result or start a new login")
	}
	handle := &oauthLoginJournal{journal: journal, key: key, record: oauthLoginJournalRecord{Version: 1, Scope: journal.ScopeID(), Owner: p.owner, Capture: captureID}}
	if err := handle.persist(ctx); err != nil {
		return err
	}
	p.journal = handle
	return nil
}

func (journal *oauthLoginJournal) persist(ctx context.Context) error {
	data, err := json.Marshal(journal.record)
	if err != nil {
		return errors.New("OAuth recovery record cannot be encoded")
	}
	reserved := 0
	if journal.record.Token == nil && !journal.completed {
		reserved = 2 * maxRemoteOAuthTokenBytes
	}
	entry, err := journal.journal.Store(ctx, journal.key, journal.revision, data, journal.completed, reserved)
	if err != nil {
		// A lost write acknowledgement may still have installed these exact
		// bytes. Only exact readback establishes permission to proceed.
		stored, found, readErr := journal.journal.Load(ctx, journal.key)
		if readErr != nil || !found || stored.Completed() != journal.completed || stored.ReservedBytes() != reserved || !bytes.Equal(stored.Payload(), data) {
			return errors.New("OAuth recovery record is not durably acknowledged")
		}
		entry = stored
	}
	journal.revision = entry.Revision()
	return nil
}

func (s *ConfigStore) startOAuthLoginExchange(ctx context.Context, p *oauthLoginPreparation) (func(), error) {
	if p.journal == nil {
		return func() {}, nil
	}
	release, err := p.journal.journal.AcquireOperation(ctx, p.journal.key)
	if err != nil {
		return nil, err
	}
	retained := false
	defer func() {
		if !retained {
			release()
		}
	}()
	if err := s.validateOAuthLogin(ctx, p); err != nil {
		return nil, err
	}
	if err := lockAuthenticationMutex(ctx, p.journal.mu.TryLock, p.journal.mu.Unlock); err != nil {
		return nil, err
	}
	defer p.journal.mu.Unlock()
	if p.journal.record.Started || p.journal.record.Abandoned || p.journal.completed {
		return nil, errors.New("OAuth exchange was already started; its recorded result requires explicit recovery")
	}
	p.journal.record.Started = true
	if err := p.journal.persist(ctx); err != nil {
		return nil, err
	}
	retained = true
	return release, nil
}

func (s *ConfigStore) retainOAuthLoginResult(ctx context.Context, p *oauthLoginPreparation, token *oauth.Token) error {
	registerOAuthTokenSecrets(token)
	if err := validateRemoteOAuthToken(token); err != nil {
		return err
	}
	if p.journal == nil {
		return nil
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), authenticationCompletionTimeout)
	defer cancel()
	if err := lockAuthenticationMutex(finish, p.journal.mu.TryLock, p.journal.mu.Unlock); err != nil {
		return err
	}
	defer p.journal.mu.Unlock()
	if !p.journal.record.Started || p.journal.record.Abandoned || p.journal.completed {
		return errors.New("OAuth result has no recorded exchange intent")
	}
	if p.journal.record.Token != nil && OAuthTokenCredentialID(p.journal.record.Token) != OAuthTokenCredentialID(token) {
		return errors.New("OAuth result differs from its durable observed token")
	}
	p.journal.record.Token = cloneOAuthToken(token)
	// A returned token is already an external effect. Caller cancellation must
	// not erase it, while store revocation still cancels the final write.
	return p.journal.persist(finish)
}

// DiscardUnstartedJournal releases only a preparation that never attempted an
// exchange. Once started, cancellation keeps the original evidence recoverable.
func (prepared OAuthLoginPreparation) DiscardUnstartedJournal(ctx context.Context) error {
	if prepared.state == nil || prepared.state.journal == nil {
		return nil
	}
	journal := prepared.state.journal
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), authenticationCompletionTimeout)
	defer cancel()
	if err := lockAuthenticationMutex(finish, journal.mu.TryLock, journal.mu.Unlock); err != nil {
		return err
	}
	defer journal.mu.Unlock()
	if journal.record.Started || journal.completed {
		return nil
	}
	journal.completed = true
	return journal.persist(finish)
}

// AcknowledgeJournalCommit records a verified local save, never remote runtime
// acknowledgement. A historical transaction must contain this exact token.
func (authorized AuthorizedOAuthPreparation) AcknowledgeJournalCommit(ctx context.Context, result AuthenticationMutationResult) error {
	state := authorized.state
	if state == nil || state.preparation == nil || state.preparation.journal == nil {
		return nil
	}
	p := state.preparation
	if !result.ConfigSaved || !result.RuntimePublished || result.After.runtime.config == nil || p.owner.AccountNamespace != "" && !result.AccountsSaved {
		return errors.New("OAuth journal completion requires its exact local commit")
	}
	provider, found := result.After.runtime.config.authenticationCollectionProvider(p.owner.ProviderID)
	owner, active := result.After.runtime.ProviderOwner(p.owner.ProviderID)
	if !found || !active || owner != p.owner || OAuthTokenCredentialID(provider.OAuthToken) != OAuthTokenCredentialID(state.provider.OAuthToken) {
		return errors.New("OAuth journal completion token differs from the transaction")
	}
	journal := p.journal
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), authenticationCompletionTimeout)
	defer cancel()
	if err := lockAuthenticationMutex(finish, journal.mu.TryLock, journal.mu.Unlock); err != nil {
		return err
	}
	defer journal.mu.Unlock()
	if journal.record.Token == nil || OAuthTokenCredentialID(journal.record.Token) != OAuthTokenCredentialID(state.provider.OAuthToken) {
		return errors.New("OAuth journal has no matching observed token")
	}
	journal.completed = true
	return journal.persist(finish)
}

// OAuthRecoverySummary is owner-side metadata. Public service projections omit
// the account namespace. A recorded token is not a save or acknowledgement.
type OAuthRecoverySummary struct {
	OriginalWorkspaceID string
	OperationID         string
	Owner               providerregistry.RegistrationOwner
	State               string
	Abandoned           bool
}

func (s *ConfigStore) PendingOAuthLoginResults(ctx context.Context, workspaceID string) ([]OAuthRecoverySummary, error) {
	return s.pendingOAuthLoginResults(ctx, workspaceID, false)
}

// PendingOAuthLoginResultsForScope enumerates prior workspace incarnations only
// within the same captured owning configuration paths. No old workspace becomes
// the current target, and this metadata authorizes no exchange or local write.
func (s *ConfigStore) PendingOAuthLoginResultsForScope(ctx context.Context) ([]OAuthRecoverySummary, error) {
	return s.pendingOAuthLoginResults(ctx, "", true)
}
func (s *ConfigStore) pendingOAuthLoginResults(ctx context.Context, workspaceID string, all bool) ([]OAuthRecoverySummary, error) {
	journal, err := s.CaptureAuthenticationJournal(ctx)
	if err != nil {
		return nil, err
	}
	var keys []AuthenticationJournalKey
	if all {
		keys, err = journal.AllKeys(ctx, AuthenticationJournalOAuth)
	} else {
		keys, err = journal.Keys(ctx, AuthenticationJournalOAuth, workspaceID)
	}
	if err != nil {
		return nil, err
	}
	var result []OAuthRecoverySummary
	for _, key := range keys {
		entry, found, err := journal.Load(ctx, key)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		// Filter captured path scope before exposing even the old workspace or
		// operation identity. Legacy records with no scope are not reassigned.
		if gjson.GetBytes(entry.Payload(), "scope").String() != journal.ScopeID() {
			continue
		}
		record, err := decodeOAuthLoginJournal(entry)
		if err != nil {
			return nil, err
		}
		if entry.Completed() && !record.Abandoned {
			continue
		}
		state := "not-started"
		if record.Started {
			state = "exchange-outcome-unknown"
		}
		if record.Token != nil {
			state = "token-result-recorded"
		}
		result = append(result, OAuthRecoverySummary{OriginalWorkspaceID: key.WorkspaceID, OperationID: key.OperationID, Owner: record.Owner, State: state, Abandoned: record.Abandoned})
	}
	slices.SortFunc(result, func(a, b OAuthRecoverySummary) int {
		if a.OriginalWorkspaceID < b.OriginalWorkspaceID {
			return -1
		}
		if a.OriginalWorkspaceID > b.OriginalWorkspaceID {
			return 1
		}
		if a.OperationID < b.OperationID {
			return -1
		}
		if a.OperationID > b.OperationID {
			return 1
		}
		return 0
	})
	return result, nil
}

// RecoverOAuthLoginResult is an explicit owner-side action against a fresh
// capture. It performs no exchange and cannot adopt a different current token.
func (s *ConfigStore) RecoverOAuthLoginResult(ctx context.Context, before AuthenticationCapture, owner providerregistry.RegistrationOwner, originalWorkspaceID, operationID string) (AuthorizedOAuthPreparation, error) {
	key := AuthenticationJournalKey{Kind: AuthenticationJournalOAuth, WorkspaceID: originalWorkspaceID, OperationID: operationID}
	journal, err := s.CaptureAuthenticationJournal(ctx)
	if err != nil {
		return AuthorizedOAuthPreparation{}, err
	}
	entry, found, err := journal.Load(ctx, key)
	if err != nil {
		return AuthorizedOAuthPreparation{}, err
	}
	if !found || entry.Completed() {
		return AuthorizedOAuthPreparation{}, errors.New("OAuth operation has no durable result")
	}
	record, err := decodeOAuthLoginJournal(entry)
	if err != nil {
		return AuthorizedOAuthPreparation{}, err
	}
	if record.Scope == "" || record.Scope != journal.ScopeID() {
		return AuthorizedOAuthPreparation{}, errors.New("OAuth recovery belongs to a different captured configuration scope")
	}
	if record.Owner != owner || !record.Started || record.Token == nil {
		return AuthorizedOAuthPreparation{}, errors.New("OAuth exchange has no recoverable observed token; start a new explicit login")
	}
	// Deliberately omit the original journal context while constructing a new
	// private preparation; it is bound to the recorded token below, not Begin.
	ctx = context.WithValue(ctx, authenticationOperationContextKey{}, struct{}{})
	prepared, err := s.PrepareOAuthLogin(ctx, before, owner)
	if err != nil {
		return AuthorizedOAuthPreparation{}, err
	}
	captureID, err := oauthLoginCaptureID(prepared.state)
	if err != nil || captureID != record.Capture {
		return AuthorizedOAuthPreparation{}, errors.New("OAuth recovery captured owner inputs changed; reconcile saved state or start a new login")
	}
	prepared.state.journal = &oauthLoginJournal{journal: journal, key: key, revision: entry.Revision(), record: record}
	return s.finishOAuthAuthorization(s.oauthLoginContext(ctx, prepared.state), prepared.state, cloneOAuthToken(record.Token))
}
