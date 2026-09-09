package config

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/providerregistry"
)

// AbandonOAuthLoginResult releases a tokenless preparation or unknown exchange's
// reservation. Its original not-started/unknown state remains retained until
// bounded completed-history pruning. It never discards a recorded token or
// performs a provider exchange. A nonempty result is the durably saved disposition.
func (s *ConfigStore) AbandonOAuthLoginResult(ctx context.Context, before AuthenticationCapture, owner providerregistry.RegistrationOwner, originalWorkspaceID, operationID string) (string, error) {
	if before.runtime.IsClientOwned() || before.runtime.publicationStore != s {
		return "", errors.New("OAuth abandonment requires its owning configuration capture")
	}
	key := AuthenticationJournalKey{Kind: AuthenticationJournalOAuth, WorkspaceID: originalWorkspaceID, OperationID: operationID}
	if err := key.Validate(); err != nil {
		return "", err
	}
	journal, err := s.CaptureAuthenticationJournal(ctx)
	if err != nil {
		return "", err
	}
	// The same lease covers Started -> HTTP -> observed-token persistence in
	// every exchange path. A live result cannot be replaced by abandonment.
	release, err := journal.AcquireOperation(ctx, key)
	if err != nil {
		return "", err
	}
	defer release()
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return "", err
	}
	defer s.writeMu.RUnlock()
	if err := s.validateAuthenticationPublication(before, owner); err != nil {
		return "", err
	}
	current, err := s.captureAuthenticationJournalLocked()
	if err != nil {
		return "", err
	}
	if current.path != journal.path || current.ScopeID() != journal.ScopeID() {
		return "", errors.New("OAuth abandonment configuration scope changed")
	}
	entry, found, err := journal.Load(ctx, key)
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("original OAuth record is unavailable")
	}
	record, err := decodeOAuthLoginJournal(entry)
	if err != nil {
		return "", err
	}
	if record.Scope == "" || record.Scope != journal.ScopeID() || record.Owner != owner {
		return "", errors.New("OAuth abandonment owner or captured scope differs")
	}
	return abandonOAuthLoginJournalRecord(ctx, journal, key, entry, record, false)
}

// The caller holds the operation lease and has checked exact scope and private
// owner provenance. The optional token branch is used only by explicit local
// journal retirement; ordinary UI/service abandonment remains tokenless.
func abandonOAuthLoginJournalRecord(ctx context.Context, journal AuthenticationJournal, key AuthenticationJournalKey, entry AuthenticationJournalEntry, record oauthLoginJournalRecord, discardRecordedToken bool) (string, error) {
	disposition := "not-started"
	if record.Started {
		disposition = "unknown"
	}
	if record.Token != nil {
		disposition = "token-result-recorded"
		if !discardRecordedToken {
			return "", errors.New("recorded OAuth token recovery requires explicit token-discard retirement")
		}
	}
	if record.Abandoned {
		return disposition, nil
	}
	if entry.Completed() {
		return "", errors.New("original OAuth operation already completed; retirement is not applicable")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	record.Abandoned = true
	record.TokenRecoveryAbandoned = record.Token != nil
	handle := oauthLoginJournal{journal: journal, key: key, revision: entry.Revision(), record: record, completed: true}
	if err := handle.persist(ctx); err != nil {
		return "", err
	}
	return disposition, nil
}
