package providerauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
)

func (s *Service) BeginOAuthLogin(ctx context.Context, request OAuthLoginRequest) (OAuthLoginState, error) {
	return s.beginOAuthLogin(ctx, request, nil, nil, nil)
}

func (s *Service) BeginOAuthLoginForAccepted(ctx context.Context, request OAuthLoginRequest, accepted config.RemoteRuntimeProposal, view *config.Config) (OAuthLoginState, error) {
	return s.beginOAuthLogin(ctx, request, &accepted, view, nil)
}

func (s *Service) beginOAuthLogin(ctx context.Context, request OAuthLoginRequest, accepted *config.RemoteRuntimeProposal, view *config.Config, recovery *OAuthLoginRecovery) (OAuthLoginState, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	if err := request.Validate(); err != nil {
		return OAuthLoginState{}, err
	}
	if request.Target.WorkspaceID != s.workspaceID || request.Target.Generation.Epoch != s.epoch {
		return OAuthLoginState{}, ErrStale
	}
	if err := s.acquire(ctx); err != nil {
		return OAuthLoginState{}, err
	}
	defer func() { <-s.gate }()
	if login, found := s.logins[request.LoginID]; found {
		if login.state.Login != request || !sameOAuthRecovery(login.state.Recovery, recovery) {
			return OAuthLoginState{}, ErrOperationConflict
		}
		return login.snapshot()
	}
	if _, found := s.receipts[request.OperationID]; found {
		return OAuthLoginState{}, ErrOperationConflict
	}
	for _, existing := range s.logins {
		if existing.state.Login.OperationID == request.OperationID {
			return OAuthLoginState{}, ErrOperationConflict
		}
	}
	snapshot, before, err := s.capture(ctx, accepted, view)
	if err != nil {
		return OAuthLoginState{}, err
	}
	if snapshot.Generation != request.Target.Generation {
		return OAuthLoginState{}, ErrStale
	}
	var owner providerregistry.RegistrationOwner
	for _, provider := range before.Providers() {
		if PublicOwner(provider.Owner) == request.Target.Owner {
			owner = provider.Owner
			break
		}
	}
	if owner.ProviderID == "" {
		return OAuthLoginState{}, ErrOwner
	}
	if s.sequence == math.MaxUint64 {
		return OAuthLoginState{}, errors.New("authentication generation exhausted; reopen the workspace")
	}
	// Consume admission before any provider effect. The request context owns
	// only this reply; the admitted session belongs to the workspace lifetime.
	s.sequence++
	lifetime, cancel := context.WithCancel(s.lifetime)
	login := &oauthLoginSession{ctx: lifetime, cancel: cancel, state: OAuthLoginState{Login: request, Recovery: recovery, Sequence: 1, Phase: OAuthLoginPreparing}, changed: make(chan struct{}), before: before, owner: owner, admittedSequence: s.sequence}
	if len(s.loginIDs) == mutationReceiptLimit {
		oldest := s.logins[s.loginIDs[0]]
		oldest.fail(context.Canceled)
		delete(s.logins, s.loginIDs[0])
		s.loginIDs = s.loginIDs[1:]
	}
	s.loginIDs = append(s.loginIDs, request.LoginID)
	s.logins[request.LoginID] = login
	s.startOAuthWork(login, func() error { return s.initializeOAuthLogin(login) })
	return login.snapshot()
}

// lookupOAuthLogin is a retained-session read. It deliberately does not replace
// the initiating target with the current generation or reopen account files.
func (s *Service) lookupOAuthLogin(ref OAuthLoginRef) (*oauthLoginSession, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if ref.Target.WorkspaceID != s.workspaceID || ref.Target.Generation.Epoch != s.epoch {
		return nil, ErrStale
	}
	login, found := s.logins[ref.LoginID]
	if !found {
		return nil, ErrOAuthLoginUnavailable
	}
	if login.state.Login != ref {
		return nil, ErrOperationConflict
	}
	return login, nil
}

// WaitOAuthLogin returns immediately for a newer state or terminal phase. A
// canceled wait has no effect on the retained login or a running exchange.
func (s *Service) WaitOAuthLogin(ctx context.Context, ref OAuthLoginRef, after uint64) (OAuthLoginState, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	if err := s.acquire(ctx); err != nil {
		return OAuthLoginState{}, err
	}
	login, err := s.lookupOAuthLogin(ref)
	<-s.gate
	if err != nil {
		return OAuthLoginState{}, err
	}
	for {
		login.mu.Lock()
		state, failure, changed := cloneOAuthLoginState(login.state), login.err, login.changed
		login.mu.Unlock()
		if after > state.Sequence {
			return OAuthLoginState{}, errors.New("OAuth login wait sequence is ahead of the owner")
		}
		if state.Sequence > after || oauthLoginTerminal(state.Phase) {
			return state, failure
		}
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case <-changed:
		}
	}
}

func (s *Service) BindOAuthLogin(ctx context.Context, request OAuthLoginBindRequest) (OAuthLoginState, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	if err := request.Validate(); err != nil {
		return OAuthLoginState{}, err
	}
	if err := s.acquire(ctx); err != nil {
		return OAuthLoginState{}, err
	}
	defer func() { <-s.gate }()
	login, err := s.lookupOAuthLogin(request.Login)
	if err != nil {
		return OAuthLoginState{}, err
	}
	login.mu.Lock()
	if login.binding != nil {
		matches := *login.binding == request
		login.mu.Unlock()
		if !matches {
			return OAuthLoginState{}, ErrOperationConflict
		}
		return login.snapshot()
	}
	if login.state.Phase != OAuthLoginWaitingLoopback {
		login.mu.Unlock()
		return OAuthLoginState{}, errors.New("OAuth login is not waiting for a callback binding")
	}
	if login.state.Callback.Mode == "loopback-fixed" && request.Port != login.state.Callback.Port {
		login.mu.Unlock()
		return OAuthLoginState{}, errors.New("OAuth callback port does not match its owner")
	}
	copy := request
	login.binding = &copy
	prepared := login.preparation
	login.publishLocked(OAuthLoginPreparing, nil, "", "")
	login.mu.Unlock()
	s.startOAuthWork(login, func() error { return s.prepareOAuthCode(login, prepared, request.Port) })
	return login.snapshot()
}

func (s *Service) SubmitOAuthLoginCode(ctx context.Context, request OAuthLoginCodeRequest) (OAuthLoginState, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	if err := request.Validate(); err != nil {
		return OAuthLoginState{}, err
	}
	if err := s.acquire(ctx); err != nil {
		return OAuthLoginState{}, err
	}
	defer func() { <-s.gate }()
	login, err := s.lookupOAuthLogin(request.Login)
	if err != nil {
		return OAuthLoginState{}, err
	}
	login.mu.Lock()
	if login.submission != nil {
		identity := request
		identity.Input = ""
		matches := *login.submission == identity && login.submissionDigest == sha256.Sum256([]byte(request.Input))
		login.mu.Unlock()
		if !matches {
			return OAuthLoginState{}, ErrOperationConflict
		}
		return login.snapshot()
	}
	if login.state.Phase != OAuthLoginWaitingBrowser && login.state.Phase != OAuthLoginWaitingCode {
		login.mu.Unlock()
		return OAuthLoginState{}, errors.New("OAuth login is not waiting for a code")
	}
	copy := request
	copy.Input = ""
	login.submission = &copy
	login.submissionDigest = sha256.Sum256([]byte(request.Input))
	code := login.code
	login.publishLocked(OAuthLoginAuthorizing, nil, "", "")
	login.mu.Unlock()
	s.startOAuthWork(login, func() error {
		authorized, err := s.store.ExchangeOAuthCode(login.ctx, code, request.Input)
		if err != nil {
			return err
		}
		login.authorize(authorized)
		return nil
	})
	return login.snapshot()
}

func (s *Service) CancelOAuthLogin(ctx context.Context, ref OAuthLoginRef) (OAuthLoginState, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	if err := s.acquire(ctx); err != nil {
		return OAuthLoginState{}, err
	}
	defer func() { <-s.gate }()
	login, err := s.lookupOAuthLogin(ref)
	if err != nil {
		return OAuthLoginState{}, err
	}
	login.mu.Lock()
	if oauthLoginTerminal(login.state.Phase) || login.state.Phase == OAuthLoginCommitting {
		login.mu.Unlock()
		return login.snapshot()
	}
	login.err = &mutationError{public: ErrOAuthLogin, cause: context.Canceled}
	login.publishLocked(OAuthLoginCanceled, nil, "", "")
	login.mu.Unlock()
	login.cancel()
	login.clearPrivate()
	return login.snapshot()
}

func (s *Service) oauthOperationReserved(operationID string) bool {
	for _, login := range s.logins {
		if login.state.Login.OperationID == operationID {
			return true
		}
	}
	return false
}
