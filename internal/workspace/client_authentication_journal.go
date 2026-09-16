package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
)

// These records stay in the owning client's captured private journal. Public
// history below projects IDs and progress only; proposals, hashes and namespaces
// never travel through authentication status, UI messages, or logs.
type clientAuthenticationJournalRecord struct {
	Version                      int
	Connection, Principal, Scope string
	Original                     *clientAuthenticationOriginalRecord `json:",omitempty"`
	Review                       *clientAuthenticationReviewRecord   `json:",omitempty"`
}
type clientAuthenticationOriginalRecord struct {
	Abandon                                             *ProviderAuthenticationAbandonRequest `json:",omitempty"`
	Request                                             clientAuthenticationStoredRequest
	Base                                                config.RemoteRuntimeProposal
	Outcome                                             providerauth.MutationOutcome
	Owner                                               providerregistry.RegistrationOwner
	Removed                                             []providerregistry.RegistrationOwner
	Proposal                                            *config.RemoteRuntimeProposal `json:",omitempty"`
	Observation                                         string
	LocalFinished, Failed, Acknowledged, Adopted        bool
	RemoteRejected                                      bool
	RecoverySequence, ReviewSequence                    uint64
	ReconciledBy, PendingReview, SavedStateSupersededBy string
	RemovalSuccessor                                    string
	RemovalActive, RemovalAdmitted                      bool
	OAuthTokenID, CredentialEffectID                    string
	Recoveries                                          []clientAuthenticationStoredRecovery
}
type clientAuthenticationStoredRequest struct {
	OperationID                                   string
	Target                                        providerauth.Target
	AccountID, RemovedAccountID, CheckID, LoginID string
	Logout                                        bool
}
type clientAuthenticationStoredRecovery struct {
	Request clientAuthenticationRecoveryRequest
	Failed  bool
}
type clientAuthenticationReviewRecord struct {
	Abandon                *ProviderAuthenticationReviewAbandonRequest `json:",omitempty"`
	SupersededByReview     string                                      `json:",omitempty"`
	OriginalAbandonedBy    string                                      `json:",omitempty"`
	Request                clientAuthenticationReviewRequest
	Summary                clientAuthenticationReviewSummary
	Owner                  providerregistry.RegistrationOwner
	Generation             providerauth.Generation
	Base, Cache, Accepted  config.RemoteAuthority
	PriorRemoved, Removed  []providerregistry.RegistrationOwner
	Proposal               *config.RemoteRuntimeProposal `json:",omitempty"`
	Observation            string
	Failed                 bool
	SavedStateSupersededBy string
	Apply                  *clientAuthenticationStoredApply `json:",omitempty"`
}
type clientAuthenticationStoredApply struct {
	Request     clientAuthenticationApplyRequest
	Outcome     clientAuthenticationReconciliationOutcome
	Failed, Put bool
}

func (clientAuthenticationJournalRecord) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private client authentication journal record]"))
}

func clientAuthenticationStored(r clientAuthenticationRequest) clientAuthenticationStoredRequest {
	return clientAuthenticationStoredRequest{r.operationID, r.target, r.accountID, r.removedAccountID, r.checkID, r.loginID, r.logout}
}

func (r clientAuthenticationStoredRequest) original() clientAuthenticationRequest {
	return clientAuthenticationRequest{operationID: r.OperationID, target: r.Target, accountID: r.AccountID, removedAccountID: r.RemovedAccountID, checkID: r.CheckID, loginID: r.LoginID, logout: r.Logout}
}

func (r clientAuthenticationStoredRequest) validate() error {
	// Removal retains its account in both fields, matching the original
	// mutation request. Equal IDs describe one removal, not a second switch.
	// Preserve that v1 representation and reject any conflicting selection.
	if r.RemovedAccountID != "" && r.AccountID != "" && r.AccountID != r.RemovedAccountID {
		return errors.New("invalid recorded authentication action")
	}
	n := 0
	for _, selected := range []bool{r.AccountID != "" && r.RemovedAccountID == "", r.RemovedAccountID != "", r.CheckID != "", r.LoginID != "", r.Logout} {
		if selected {
			n++
		}
	}
	if n != 1 {
		return errors.New("invalid recorded authentication action")
	}
	switch {
	case r.LoginID != "":
		return (providerauth.OAuthLoginRef{LoginID: r.LoginID, OperationID: r.OperationID, Target: r.Target}).Validate()
	case r.CheckID != "":
		return (providerauth.APIKeySaveRequest{OperationID: r.OperationID, Target: r.Target, CheckID: r.CheckID}).Validate()
	case r.RemovedAccountID != "":
		return (providerauth.RemoveRequest{OperationID: r.OperationID, Target: r.Target, AccountID: r.RemovedAccountID}).Validate()
	case r.Logout:
		return (providerauth.LogoutRequest{OperationID: r.OperationID, Target: r.Target}).Validate()
	default:
		return (providerauth.SwitchRequest{OperationID: r.OperationID, Target: r.Target, AccountID: r.AccountID}).Validate()
	}
}

func journalOwners(values map[providerregistry.RegistrationOwner]bool) []providerregistry.RegistrationOwner {
	var result []providerregistry.RegistrationOwner
	for owner, removed := range values {
		if removed {
			result = append(result, owner)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, _ := json.Marshal(result[i])
		b, _ := json.Marshal(result[j])
		return string(a) < string(b)
	})
	return result
}

func journalOwnerMap(values []providerregistry.RegistrationOwner) map[providerregistry.RegistrationOwner]bool {
	result := map[providerregistry.RegistrationOwner]bool{}
	for _, owner := range values {
		result[owner] = true
	}
	return result
}

func (a *clientAuthority) authenticationJournalKey(workspace, kind, id string) config.AuthenticationJournalKey {
	data, _ := json.Marshal([]string{a.authenticationConnection, a.principal, a.authenticationScope, kind, id})
	digest := sha256.Sum256(data)
	return config.AuthenticationJournalKey{Kind: config.AuthenticationJournalClient, WorkspaceID: workspace, OperationID: hex.EncodeToString(digest[:])}
}

// Callers hold only the authority mutex, never the workspace cache mutex.
// Each read checks durable revisions so a second store cannot overwrite a
// receipt this process has not observed. Store's CAS is the final write guard.
func (a *clientAuthority) loadAuthenticationJournal(ctx context.Context, workspace string) error {
	if a.authenticationConnection == "" {
		return nil
	} // Legacy unbound/IPC clients have no saved TLS identity.
	if a.authenticationJournal == nil {
		journal, err := a.store.CaptureAuthenticationJournal(ctx)
		if err != nil {
			return err
		}
		a.authenticationJournal = &journal
		a.authenticationScope = journal.ScopeID()
	}
	entries, err := a.authenticationJournal.Entries(ctx, config.AuthenticationJournalClient, workspace)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		key := entry.Key()
		var record clientAuthenticationJournalRecord
		decoder := json.NewDecoder(bytes.NewReader(entry.Payload()))
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Version != 1 || (record.Original == nil) == (record.Review == nil) {
			return errors.New("recorded client authentication data is invalid")
		}
		canonical, encodeErr := json.Marshal(record)
		if encodeErr != nil || !bytes.Equal(canonical, entry.Payload()) {
			return errors.New("recorded authentication data has ambiguous or noncanonical fields")
		}
		if record.Connection != a.authenticationConnection || record.Principal != a.principal || record.Scope != a.authenticationScope {
			continue
		}
		if o := record.Original; o != nil {
			if o.Request.validate() != nil || o.Request.Target.WorkspaceID != workspace || key != a.authenticationJournalKey(workspace, "original", o.Request.OperationID) || o.Outcome.OperationID != o.Request.OperationID || o.Outcome.Previous != o.Request.Target || providerauth.PublicOwner(o.Owner) != o.Request.Target.Owner || o.Adopted && !o.Acknowledged || o.RemoteRejected && (!o.Failed || o.Acknowledged || o.Proposal == nil) {
				return errors.New("recorded authentication intent is invalid")
			}
			if o.Abandon != nil && (o.Abandon.Validate() != nil || o.Abandon.OperationID != o.Request.OperationID || o.Abandon.Target != o.Request.Target || !entry.Completed()) {
				return errors.New("recorded authentication abandonment is invalid")
			}
			if existing := a.authenticationReceipts[o.Request.OperationID]; existing != nil {
				if existing.request != o.Request.original() || existing.owner != o.Owner || existing.journalRevision > entry.Revision() {
					return errors.New("recorded authentication operation identity changed")
				}
				if existing.journalRevision == entry.Revision() {
					continue
				}
			}
			if err := validateJournalProposal(o.Base); err != nil {
				return err
			}
			if o.Proposal != nil {
				if err := validateJournalProposal(*o.Proposal); err != nil {
					return err
				}
			}
			r := &clientAuthenticationReceipt{abandon: o.Abandon, request: o.Request.original(), principal: record.Principal, base: o.Base, outcome: o.Outcome, owner: o.Owner, removed: journalOwnerMap(o.Removed), proposal: o.Proposal, observation: o.Observation, localFinished: o.LocalFinished, acknowledged: o.Acknowledged, adopted: o.Adopted, remoteRejected: o.RemoteRejected, recoverySequence: o.RecoverySequence, reviewSequence: o.ReviewSequence, reconciledBy: o.ReconciledBy, pendingReview: o.PendingReview, savedStateSupersededBy: o.SavedStateSupersededBy, removalSuccessor: o.RemovalSuccessor, removalActive: o.RemovalActive, removalAdmitted: o.RemovalAdmitted, oauthTokenID: o.OAuthTokenID, credentialEffectID: o.CredentialEffectID, journalRevision: entry.Revision(), journalCompleted: entry.Completed(), restored: true}
			if o.RemoteRejected {
				r.err = errors.New("recorded authentication publication was rejected by the receiver")
			} else if o.Failed || !o.LocalFinished {
				r.err = errors.New("recorded authentication result requires explicit recovery or saved-state review")
			}
			a.retainClientAuthentication(r)
			for _, recovery := range o.Recoveries {
				if recovery.Request.validate() != nil || recovery.Request.OperationID != o.Request.OperationID || recovery.Request.Target != o.Request.Target {
					return errors.New("recorded authentication recovery is invalid")
				}
				rr := &clientAuthenticationRecoveryReceipt{request: recovery.Request, proposal: r.proposal}
				if recovery.Failed {
					rr.err = providerauth.ErrReceiptUnverified
				}
				a.retainClientAuthenticationRecovery(rr)
			}
		} else {
			r := record.Review
			if r.Request.validate() != nil || r.Request.target().WorkspaceID != workspace || key != a.authenticationJournalKey(workspace, "review", r.Request.ReviewID) || r.Summary.ReviewID != r.Request.ReviewID || providerauth.PublicOwner(r.Owner) != r.Request.target().Owner {
				return errors.New("recorded authentication review is invalid")
			}
			if r.SupersededByReview != "" && (validateAuthenticationReviewIDs(r.Request.target(), r.SupersededByReview) != nil || !entry.Completed()) {
				return errors.New("recorded review supersession is invalid")
			}
			if r.OriginalAbandonedBy != "" && (r.Request.FreshSaved || validateAuthenticationReviewIDs(r.Request.target(), r.OriginalAbandonedBy) != nil || !entry.Completed()) {
				return errors.New("recorded original-review abandonment is invalid")
			}
			if r.Abandon != nil && (r.Abandon.Validate() != nil || r.Abandon.Review != ProviderAuthenticationReviewRequest(r.Request) || r.Abandon.PreviewID != r.Summary.PreviewID || r.Apply == nil || !r.Apply.Put || r.Apply.Outcome.Adopted || !entry.Completed()) {
				return errors.New("recorded review abandonment is invalid")
			}
			if existing := a.authenticationReviews[r.Request.ReviewID]; existing != nil {
				if existing.request != r.Request || existing.owner != r.Owner || existing.journalRevision > entry.Revision() {
					return errors.New("recorded authentication review identity changed")
				}
				if existing.journalRevision == entry.Revision() {
					continue
				}
			}
			if r.Proposal != nil {
				if err := validateJournalProposal(*r.Proposal); err != nil {
					return err
				}
				if err := r.Generation.Validate(); err != nil {
					return err
				}
			}
			review := &clientAuthenticationReviewReceipt{abandon: r.Abandon, originalAbandonedBy: r.OriginalAbandonedBy, supersededByReview: r.SupersededByReview, request: r.Request, summary: r.Summary, owner: r.Owner, generation: r.Generation, base: r.Base, cache: r.Cache, accepted: r.Accepted, priorRemoved: journalOwnerMap(r.PriorRemoved), removed: journalOwnerMap(r.Removed), proposal: r.Proposal, observation: r.Observation, savedStateSupersededBy: r.SavedStateSupersededBy, journalRevision: entry.Revision(), journalCompleted: entry.Completed(), restored: true}
			if r.Failed {
				review.err = providerauth.ErrReceiptUnverified
			}
			if p := r.Apply; p != nil {
				if p.Request.validate() != nil || p.Request.ReviewID != r.Request.ReviewID || p.Request.PreviewID != r.Summary.PreviewID || p.Request.OperationID != r.Request.OperationID || p.Request.FreshSaved != r.Request.FreshSaved || p.Request.OriginalTarget != r.Request.OriginalTarget || p.Request.SavedTarget != r.Request.SavedTarget || p.Outcome.Validate(ProviderAuthenticationApplyRequest(p.Request)) != nil {
					return errors.New("recorded authentication apply is invalid")
				}
				review.apply = &clientAuthenticationApplyReceipt{request: p.Request, outcome: p.Outcome, put: p.Put}
				if p.Failed {
					review.apply.err = providerauth.ErrReceiptUnverified
				}
			}
			a.retainAuthenticationReview(review)
		}
	}
	for _, original := range a.authenticationReceipts {
		if original.abandon != nil {
			a.discardAbandonedOriginalPending(original)
		}
	}
	for _, review := range a.authenticationReviews {
		if review.abandon != nil {
			a.discardAbandonedReviewPending(review)
			continue
		}
		if review.apply != nil && review.apply.put && !review.request.FreshSaved && !review.apply.outcome.Adopted && review.savedStateSupersededBy == "" && review.originalAbandonedBy == "" {
			if original := a.authenticationReceipts[review.request.OperationID]; original != nil && original.request.target == review.request.OriginalTarget && original.owner == review.owner && original.abandon == nil {
				original.pendingReview = review.summary.PreviewID
			}
		}
		if review.apply == nil || !review.apply.outcome.Adopted {
			continue
		}
		if review.request.FreshSaved {
			a.supersedeAuthenticationWithSavedReview(review)
		} else if original := a.authenticationReceipts[review.request.OperationID]; original != nil && original.abandon == nil && original.owner == review.owner && original.request.target == review.request.OriginalTarget {
			original.reconciledBy = review.summary.PreviewID
		}
	}

	return nil
}

func validateJournalProposal(p config.RemoteRuntimeProposal) error {
	digest, err := config.RemoteRuntimeDigest(p)
	if err != nil || p.Version != config.RemoteRuntimeVersion || p.Revision == 0 || digest != p.Digest {
		return errors.New("recorded authentication proposal is invalid")
	}
	config.RegisterAuthenticationProposalSecrets(p)
	return nil
}

func (a *clientAuthority) storeAuthenticationJournal(ctx context.Context, key config.AuthenticationJournalKey, revision uint64, value clientAuthenticationJournalRecord, completed bool, reserved int) (uint64, error) {
	if a.authenticationJournal == nil {
		return revision, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return revision, errors.New("authentication history cannot be encoded")
	}
	entry, err := a.authenticationJournal.Store(ctx, key, revision, data, completed, reserved)
	if err != nil {
		stored, found, readErr := a.authenticationJournal.Load(ctx, key)
		if readErr != nil || !found || !bytes.Equal(stored.Payload(), data) || stored.Completed() != completed || stored.ReservedBytes() != reserved {
			return revision, errors.Join(errors.New("authentication history write is not durably acknowledged"), ctx.Err())
		}
		entry = stored
	}
	return entry.Revision(), nil
}

func (a *clientAuthority) persistAuthenticationReceipt(ctx context.Context, r *clientAuthenticationReceipt) error {
	if a.authenticationJournal == nil || r.journalCompleted {
		return nil
	}
	if r.after.SameObservation(r.after) && r.observation == "" {
		id, err := r.after.DurableObservationID()
		if err != nil {
			return err
		}
		r.observation = id
	}
	o := &clientAuthenticationOriginalRecord{Abandon: r.abandon, Request: clientAuthenticationStored(r.request), Base: r.base, Outcome: r.outcome, Owner: r.owner, Removed: journalOwners(r.removed), Proposal: r.proposal, Observation: r.observation, LocalFinished: r.localFinished, Failed: r.err != nil, Acknowledged: r.acknowledged, Adopted: r.adopted, RemoteRejected: r.remoteRejected, RecoverySequence: r.recoverySequence, ReviewSequence: r.reviewSequence, ReconciledBy: r.reconciledBy, PendingReview: r.pendingReview, SavedStateSupersededBy: r.savedStateSupersededBy, RemovalSuccessor: r.removalSuccessor, RemovalActive: r.removalActive, RemovalAdmitted: r.removalAdmitted, OAuthTokenID: r.oauthTokenID, CredentialEffectID: r.credentialEffectID}
	for _, id := range a.authenticationRecoveryIDs {
		if recovery := a.authenticationRecoveries[id]; recovery != nil && recovery.request.OperationID == r.request.operationID {
			o.Recoveries = append(o.Recoveries, clientAuthenticationStoredRecovery{recovery.request, recovery.err != nil})
		}
	}
	completed := r.abandon != nil || r.savedStateSupersededBy != "" || r.reconciledBy != "" || r.acknowledged && r.adopted || r.localFinished && !clientAuthenticationChanged(r.outcome.Progress) && r.outcome.Change == nil
	reserved := 0
	if !completed && r.proposal == nil {
		reserved = config.MaxRemoteRuntimeBytes
	}
	revision, err := a.storeAuthenticationJournal(ctx, a.authenticationJournalKey(r.request.target.WorkspaceID, "original", r.request.operationID), r.journalRevision, clientAuthenticationJournalRecord{Version: 1, Connection: a.authenticationConnection, Principal: a.principal, Scope: a.authenticationScope, Original: o}, completed, reserved)
	if err == nil {
		r.journalRevision, r.journalCompleted = revision, completed
	}
	return err
}

func (a *clientAuthority) persistAuthenticationReview(ctx context.Context, r *clientAuthenticationReviewReceipt) error {
	if a.authenticationJournal == nil || r.journalCompleted {
		return nil
	}
	if r.capture.SameObservation(r.capture) && r.observation == "" {
		id, err := r.capture.DurableObservationID()
		if err != nil {
			return err
		}
		r.observation = id
	}
	value := &clientAuthenticationReviewRecord{Abandon: r.abandon, OriginalAbandonedBy: r.originalAbandonedBy, SupersededByReview: r.supersededByReview, Request: r.request, Summary: r.summary, Owner: r.owner, Generation: r.generation, Base: r.base, Cache: r.cache, Accepted: r.accepted, PriorRemoved: journalOwners(r.priorRemoved), Removed: journalOwners(r.removed), Proposal: r.proposal, Observation: r.observation, Failed: r.err != nil, SavedStateSupersededBy: r.savedStateSupersededBy}
	if p := r.apply; p != nil {
		value.Apply = &clientAuthenticationStoredApply{Request: p.request, Outcome: p.outcome, Failed: p.err != nil, Put: p.put}
	}
	completed := r.abandon != nil || r.originalAbandonedBy != "" || r.supersededByReview != "" || r.savedStateSupersededBy != "" || r.apply != nil && r.apply.outcome.Adopted || r.err != nil && r.apply == nil || r.apply != nil && r.apply.err != nil && !r.apply.put
	revision, err := a.storeAuthenticationJournal(ctx, a.authenticationJournalKey(r.request.target().WorkspaceID, "review", r.request.ReviewID), r.journalRevision, clientAuthenticationJournalRecord{Version: 1, Connection: a.authenticationConnection, Principal: a.principal, Scope: a.authenticationScope, Review: value}, completed, 0)
	if err == nil {
		r.journalRevision, r.journalCompleted = revision, completed
	}
	return err
}

func (a *clientAuthority) finishAuthenticationJournal(ctx context.Context) error {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// Persist acknowledged reviews first. If a crash precedes the independent
	// old-receipt update, load derives the same supersession from that proof.
	for _, id := range a.authenticationReviewIDs {
		if r := a.authenticationReviews[id]; r != nil {
			if err := a.persistAuthenticationReview(finish, r); err != nil {
				return err
			}
		}
	}
	for _, id := range a.authenticationReceiptIDs {
		if r := a.authenticationReceipts[id]; r != nil {
			if err := a.persistAuthenticationReceipt(finish, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// Restored receipts retain genuine historical acknowledgement independently of
// whether current files still support adopting their former runtime view.
func (w *ClientWorkspace) restoreAuthenticationReceipt(ctx context.Context, a *clientAuthority, r *clientAuthenticationReceipt) error {
	if !r.restored || r.after.SameObservation(r.after) {
		return nil
	}
	if r.proposal == nil {
		return errors.New("recorded local operation has no collected proposal; repair or reload and review saved state")
	}
	capture, proposal, err := a.store.RestoreAuthenticationProposal(ctx, *r.proposal, r.observation, r.removed)
	if err != nil {
		return err
	}
	r.after, r.proposal = capture, &proposal
	return nil
}

func (w *ClientWorkspace) restoreAuthenticationReview(ctx context.Context, a *clientAuthority, r *clientAuthenticationReviewReceipt) error {
	if !r.restored || r.capture.SameObservation(r.capture) {
		return nil
	}
	if r.proposal == nil {
		return providerauth.ErrReceiptUnverified
	}
	capture, proposal, err := a.store.RestoreAuthenticationProposal(ctx, *r.proposal, r.observation, r.removed)
	if err != nil {
		return err
	}
	r.capture, r.proposal = capture, &proposal
	return nil
}

func (w *ClientWorkspace) adoptRestoredAuthenticationReview(ctx context.Context, a *clientAuthority, original *clientAuthenticationReceipt, review *clientAuthenticationReviewReceipt, ack *config.RemoteAuthority) (clientAuthenticationReconciliationOutcome, error) {
	apply := review.apply
	if err := w.restoreAuthenticationReview(ctx, a, review); err != nil {
		return apply.outcome, err
	}
	if err := w.lockAuthenticationReviewWorkspace(ctx); err != nil {
		return apply.outcome, err
	}
	defer w.mu.Unlock()
	if w.authority != a || w.ws.ID != review.request.target().WorkspaceID || w.ws.Authority == nil || !matchesAuthority(w.ws.Authority, a.principal, a.accepted) || !authenticationReviewOriginalCurrent(a, original, review) {
		return apply.outcome, providerauth.ErrStale
	}
	if a.accepted.Revision > review.proposal.Revision {
		return apply.outcome, errors.New("recorded review was acknowledged but a later runtime is already accepted")
	}
	if a.accepted.Revision == review.proposal.Revision && a.accepted.Digest != review.proposal.Digest {
		return apply.outcome, providerauth.ErrStale
	}
	if a.accepted.Revision < review.proposal.Revision && !matchesAuthority(&review.base, a.principal, a.accepted) {
		return apply.outcome, errors.New("recorded review no longer follows the accepted runtime")
	}
	a.accepted = *review.proposal
	a.view.Store(review.proposal.CollectionConfig())
	a.removed = journalOwnerMap(journalOwners(review.removed))
	if a.pending != nil && a.pending.Revision == review.proposal.Revision && a.pending.Digest == review.proposal.Digest {
		a.pending, a.pendingView = nil, nil
	}
	w.ws.Authority = new(config.RemoteAuthority)
	*w.ws.Authority = cloneAuthenticationReviewAuthority(*ack)
	w.appliedRefresh = w.refreshSequence.Add(1)
	w.providerAuthAppliedRead = w.providerAuthReadSequence.Add(1)
	apply.outcome.Adopted = true
	if review.request.FreshSaved {
		a.supersedeAuthenticationWithSavedReview(review)
	}
	if original != nil {
		apply.outcome.OriginalDisposition = "runtime-reconciled"
		original.reconciledBy = review.summary.PreviewID
	}
	return apply.outcome, nil
}

// One publication lane protects original commits, reviews, recovery and their
// cross-record supersession for this captured saved authority/workspace. Record
// Store keys remain operation-specific. No entrypoint nests publication leases.
func (a *clientAuthority) acquireAuthenticationPublication(ctx context.Context, workspace string) (func(), error) {
	if a.authenticationJournal == nil {
		return func() {}, nil
	}
	return a.authenticationJournal.AcquireOperation(ctx, a.authenticationJournalKey(workspace, "publication-lane", "authority"))
}

func shareAuthenticationLease(release func()) (func(), func()) {
	var remaining atomic.Int32
	remaining.Store(2)
	reference := func() func() {
		var once sync.Once
		return func() {
			once.Do(func() {
				if remaining.Add(-1) == 0 {
					release()
				}
			})
		}
	}
	return reference(), reference()
}
