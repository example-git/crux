package providerauth

import (
	"context"
	"errors"
	"math"

	"github.com/example-git/crux/internal/config"
)

func (s *Service) CompleteOAuthLogin(ctx context.Context, ref OAuthLoginRef) (MutationResult, error) {
	return s.completeOAuthLogin(ctx, ref, nil, nil)
}

func (s *Service) CompleteOAuthLoginForAccepted(ctx context.Context, ref OAuthLoginRef, accepted config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	return s.completeOAuthLogin(ctx, ref, &accepted, view)
}

// Complete consumes only this login's retained private authorization. Once
// admitted, the commit belongs to the workspace even if its HTTP reply is lost.
// The returned local receipt still needs a separate owning-client runtime ack.
func (s *Service) completeOAuthLogin(ctx context.Context, ref OAuthLoginRef, accepted *config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	initial := MutationResult{Outcome: MutationOutcome{OperationID: ref.OperationID, LoginID: ref.LoginID, Previous: ref.Target}}
	if err := ref.Validate(); err != nil {
		return initial, err
	}
	if ref.Target.WorkspaceID != s.workspaceID || ref.Target.Generation.Epoch != s.epoch {
		return initial, ErrStale
	}
	if err := s.acquire(ctx); err != nil {
		return initial, err
	}
	ownedGate := true
	defer func() {
		if ownedGate {
			<-s.gate
		}
	}()
	request := mutationRequest{operationID: ref.OperationID, target: ref.Target, loginID: ref.LoginID}
	if receipt, found := s.receipts[ref.OperationID]; found {
		if receipt.request != request {
			return initial, ErrOperationConflict
		}
		return s.replay(ctx, receipt)
	}
	login, err := s.lookupOAuthLogin(ref)
	if err != nil {
		return initial, err
	}
	login.mu.Lock()
	if login.state.Phase != OAuthLoginAuthorized {
		err := login.err
		login.mu.Unlock()
		if err == nil {
			err = errors.New("OAuth login is not authorized for completion")
		}
		return initial, err
	}
	before, owner, authorized := login.before, login.owner, login.authorized
	login.mu.Unlock()
	snapshot, current, err := s.capture(ctx, accepted, view)
	if err != nil {
		return initial, err
	}
	if !current.SameObservation(before) || snapshot.Generation.Sequence != login.admittedSequence {
		login.fail(ErrStale)
		return initial, ErrStale
	}
	if s.sequence == math.MaxUint64 {
		return initial, errors.New("authentication generation exhausted; reopen the workspace")
	}
	initial.originalOwner = owner
	login.mu.Lock()
	if login.ctx.Err() != nil {
		login.mu.Unlock()
		return initial, login.ctx.Err()
	}
	login.publishLocked(OAuthLoginCommitting, nil, "", "")
	login.mu.Unlock()
	finished := make(chan struct{})
	s.workers.Add(1)
	// Transfer the held gate to the worker. No status/mutation can interleave
	// between completion admission and retaining its exact transaction result.
	ownedGate = false
	go func() {
		defer s.workers.Done()
		defer close(finished)
		defer func() { <-s.gate }()
		commitCtx := config.ContextWithAuthenticationOperation(login.ctx, config.AuthenticationJournalKey{Kind: config.AuthenticationJournalLocal, WorkspaceID: s.workspaceID, OperationID: ref.OperationID})
		transaction, failure := s.store.CommitOAuthLogin(commitCtx, config.ScopeGlobal, authorized)
		if failure == nil {
			failure = authorized.AcknowledgeJournalCommit(login.ctx, transaction)
		}
		receipt := mutationReceipt{request: request, originalOwner: owner, outcome: initial.Outcome}
		receipt.outcome.Progress = MutationProgress{AccountRefreshed: transaction.AccountRefreshed, AccountsSaved: transaction.AccountsSaved, ConfigSaved: transaction.ConfigSaved, RuntimePublished: transaction.RuntimePublished}
		runtime, coherent := transaction.RuntimeSnapshot()
		if failure != nil || !coherent || transaction.After.SameObservation(before) {
			s.sequence++
			if failure == nil {
				failure = errors.New("OAuth login commit returned no coherent changed publication")
			}
		} else {
			var post Snapshot
			post, failure = s.observe(transaction.After)
			if failure == nil {
				var accounts AccountsState
				accounts, failure = accountsState(post, transaction.After, ref.Target.Owner)
				if failure == nil {
					receipt.outcome.Change = &Change{OperationID: ref.OperationID, Previous: ref.Target, Current: accounts, Models: publicModels(runtime)}
					failure = receipt.outcome.ValidateOAuthLogin(ref)
				}
			}
		}
		if failure == nil && owner.AccountNamespace == "" {
			receipt.oauthTokenID, failure = transaction.After.ConfiguredOAuthTokenCredentialID(owner)
		}
		if failure != nil {
			receipt.outcome.Change = nil
			receipt.err = safeMutationError(failure)
		} else {
			receipt.after, receipt.runtime = transaction.After, runtime
		}
		s.retain(receipt)
		login.mu.Lock()
		login.err = receipt.err
		phase := OAuthLoginComplete
		if receipt.err != nil {
			phase = OAuthLoginFailed
		}
		login.publishLocked(phase, nil, "", "")
		login.mu.Unlock()
		login.cancel()
		login.clearPrivate()
	}()
	select {
	case <-ctx.Done():
		return initial, ctx.Err()
	case <-finished:
	}
	// Re-enter through the exact receipt branch. No second commit is possible.
	return s.completeOAuthLogin(ctx, ref, accepted, view)
}
