package config

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/tidwall/gjson"
)

// OAuthLoginJournalScope permits only local journal metadata and explicit
// retirement. It cannot load credentials, initialize providers or publish a
// runtime. Paths are explicit original owning inputs, never receiver defaults.
type OAuthLoginJournalScope struct{ journal AuthenticationJournal }

func (OAuthLoginJournalScope) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth journal scopes are private")
}

func (OAuthLoginJournalScope) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth journal scope]"))
}

// OpenOAuthLoginJournalScope does no config evaluation or provider work. The
// workspace path may be explicitly empty if the original store captured that
// value. Scope hashing preserves the exact supplied strings.
func OpenOAuthLoginJournalScope(ctx context.Context, globalConfigData, workspaceConfig, workingDirectory string) (OAuthLoginJournalScope, error) {
	if err := ctx.Err(); err != nil {
		return OAuthLoginJournalScope{}, err
	}
	if !filepath.IsAbs(globalConfigData) || !filepath.IsAbs(workingDirectory) || workspaceConfig != "" && !filepath.IsAbs(workspaceConfig) {
		return OAuthLoginJournalScope{}, errors.New("OAuth journal scope requires absolute original paths; only workspace config may be empty")
	}
	store := &ConfigStore{globalDataPath: globalConfigData, workspacePath: workspaceConfig, workingDir: workingDirectory}
	journal, err := store.CaptureAuthenticationJournal(ctx)
	if err != nil {
		return OAuthLoginJournalScope{}, err
	}
	return OAuthLoginJournalScope{journal: journal}, nil
}

// OAuthLoginJournalMetadata contains only public original IDs and state. It
// exposes no owner, account namespace, configuration path or token fingerprint.
type OAuthLoginJournalMetadata struct {
	OriginalWorkspaceID string `json:"original_workspace_id"`
	OriginalOperationID string `json:"original_operation_id"`
	State               string `json:"state"`
	Abandoned           bool   `json:"abandoned"`
}

func oauthLoginJournalMetadata(key AuthenticationJournalKey, record oauthLoginJournalRecord) OAuthLoginJournalMetadata {
	state := "not-started"
	if record.Started {
		state = "exchange-outcome-unknown"
	}
	if record.Token != nil {
		state = "token-result-recorded"
	}
	return OAuthLoginJournalMetadata{OriginalWorkspaceID: key.WorkspaceID, OriginalOperationID: key.OperationID, State: state, Abandoned: record.Abandoned}
}

func (scope OAuthLoginJournalScope) Pending(ctx context.Context) ([]OAuthLoginJournalMetadata, error) {
	keys, err := scope.journal.AllKeys(ctx, AuthenticationJournalOAuth)
	if err != nil {
		return nil, err
	}
	results := []OAuthLoginJournalMetadata{}
	for _, key := range keys {
		entry, found, err := scope.journal.Load(ctx, key)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		capturedScope := gjson.GetBytes(entry.Payload(), "scope").String()
		if capturedScope == "" {
			return nil, errors.New("an OAuth journal record has no captured scope; it cannot be reassigned or retired by this command")
		}
		if capturedScope != scope.journal.ScopeID() {
			continue
		}
		record, err := decodeOAuthLoginJournal(entry)
		if err != nil {
			return nil, err
		}
		if !entry.Completed() || record.Abandoned {
			results = append(results, oauthLoginJournalMetadata(key, record))
		}
	}
	slices.SortFunc(results, func(a, b OAuthLoginJournalMetadata) int {
		if a.OriginalWorkspaceID < b.OriginalWorkspaceID {
			return -1
		}
		if a.OriginalWorkspaceID > b.OriginalWorkspaceID {
			return 1
		}
		if a.OriginalOperationID < b.OriginalOperationID {
			return -1
		}
		if a.OriginalOperationID > b.OriginalOperationID {
			return 1
		}
		return 0
	})
	return results, nil
}

// OAuthLoginJournalRetirement retains the full private original owner and
// captured input proof. It grants no exchange, save or publication authority.
type OAuthLoginJournalRetirement struct {
	journal  AuthenticationJournal
	key      AuthenticationJournalKey
	original oauthLoginJournalRecord
}

func (OAuthLoginJournalRetirement) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth journal retirement captures are private")
}

func (OAuthLoginJournalRetirement) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth journal retirement capture]"))
}

func (scope OAuthLoginJournalScope) CaptureRetirement(ctx context.Context, originalWorkspaceID, operationID string) (OAuthLoginJournalRetirement, error) {
	key := AuthenticationJournalKey{Kind: AuthenticationJournalOAuth, WorkspaceID: originalWorkspaceID, OperationID: operationID}
	entry, found, err := scope.journal.Load(ctx, key)
	if err != nil {
		return OAuthLoginJournalRetirement{}, err
	}
	if !found {
		return OAuthLoginJournalRetirement{}, errors.New("original OAuth record is unavailable")
	}
	record, err := decodeOAuthLoginJournal(entry)
	if err != nil {
		return OAuthLoginJournalRetirement{}, err
	}
	if record.Scope == "" || record.Scope != scope.journal.ScopeID() {
		return OAuthLoginJournalRetirement{}, errors.New("original OAuth record has a missing or different captured scope")
	}
	if entry.Completed() && !record.Abandoned {
		return OAuthLoginJournalRetirement{}, errors.New("original OAuth record already completed; retirement is not applicable")
	}
	return OAuthLoginJournalRetirement{journal: scope.journal, key: key, original: record}, nil
}

func (capture OAuthLoginJournalRetirement) Retire(ctx context.Context, discardRecordedToken bool) (OAuthLoginJournalMetadata, error) {
	journal, key := capture.journal, capture.key
	release, err := journal.AcquireOperation(ctx, key)
	if err != nil {
		return OAuthLoginJournalMetadata{}, err
	}
	defer release()
	entry, found, err := journal.Load(ctx, key)
	if err != nil {
		return OAuthLoginJournalMetadata{}, err
	}
	if !found {
		return OAuthLoginJournalMetadata{}, errors.New("original OAuth record is unavailable")
	}
	record, err := decodeOAuthLoginJournal(entry)
	if err != nil {
		return OAuthLoginJournalMetadata{}, err
	}
	if record.Scope == "" || record.Scope != journal.ScopeID() || record.Scope != capture.original.Scope || record.Owner != capture.original.Owner || record.Capture != capture.original.Capture {
		return OAuthLoginJournalMetadata{}, errors.New("OAuth retirement original owner or captured scope changed")
	}
	if capture.original.Token != nil && OAuthTokenCredentialID(capture.original.Token) != OAuthTokenCredentialID(record.Token) {
		return OAuthLoginJournalMetadata{}, errors.New("OAuth retirement observed token changed")
	}
	_, err = abandonOAuthLoginJournalRecord(ctx, journal, key, entry, record, discardRecordedToken)
	if err != nil {
		return OAuthLoginJournalMetadata{}, err
	}
	result := oauthLoginJournalMetadata(key, record)
	result.Abandoned = true
	return result, nil
}
