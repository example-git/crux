package providerauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"sync"

	"github.com/example-git/crux/internal/config"
)

// Service is owned by one workspace incarnation. The public sequence only
// identifies observed state; it is never a hash of a credential or file.
type Service struct {
	store       *config.ConfigStore
	workspaceID string
	epoch       string
	gate        chan struct{}
	sequence    uint64
	last        config.AuthenticationCapture
	mutations   authenticationMutator
	receipts    map[string]mutationReceipt
	receiptIDs  []string
	apiKeys     checkedAPIKeyStore
	keyChecks   map[string]apiKeyCheckReceipt
	keyCheckIDs []string
	lifetime    context.Context
	cancel      context.CancelFunc
	closeOnce   sync.Once
	workers     sync.WaitGroup
	logins      map[string]*oauthLoginSession
	loginIDs    []string
	reloads     map[string]reloadReceipt
	reloadIDs   []string
}

func New(store *config.ConfigStore, workspaceID string) *Service {
	return NewWithContext(context.Background(), store, workspaceID)
}

func NewWithContext(lifetime context.Context, store *config.ConfigStore, workspaceID string) *Service {
	var epoch [16]byte
	// crypto/rand.Read either fills the buffer or terminates the process on a
	// broken OS entropy source. No predictable authentication incarnation fallback.
	_, _ = rand.Read(epoch[:])
	ctx, cancel := context.WithCancel(lifetime)
	service := &Service{store: store, workspaceID: workspaceID, epoch: hex.EncodeToString(epoch[:]), gate: make(chan struct{}, 1), mutations: store, receipts: map[string]mutationReceipt{}, apiKeys: store, keyChecks: map[string]apiKeyCheckReceipt{}, lifetime: ctx, cancel: cancel, logins: map[string]*oauthLoginSession{}}
	context.AfterFunc(ctx, service.Close)
	return service
}

func (s *Service) Status(ctx context.Context) (Snapshot, error) {
	return s.status(ctx, nil, nil)
}

func (s *Service) StatusForAccepted(ctx context.Context, accepted config.RemoteRuntimeProposal, view *config.Config) (Snapshot, error) {
	return s.status(ctx, &accepted, view)
}

func (s *Service) Accounts(ctx context.Context, target Target) (AccountsState, error) {
	return s.accounts(ctx, target, nil, nil)
}

func (s *Service) AccountsForAccepted(ctx context.Context, target Target, accepted config.RemoteRuntimeProposal, view *config.Config) (AccountsState, error) {
	return s.accounts(ctx, target, &accepted, view)
}

func (s *Service) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.lifetime.Done():
		return s.lifetime.Err()
	case <-ctx.Done():
		return ctx.Err()
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.gate
			return err
		}
		if err := s.lifetime.Err(); err != nil {
			<-s.gate
			return err
		}
		return nil
	}
}

func (s *Service) capture(ctx context.Context, accepted *config.RemoteRuntimeProposal, view *config.Config) (Snapshot, config.AuthenticationCapture, error) {
	if s.store == nil || !validText(s.workspaceID, 512, true) {
		return Snapshot{}, config.AuthenticationCapture{}, errors.New("authentication workspace is unavailable")
	}
	capture, err := s.store.CaptureAuthentication(ctx)
	if err != nil {
		return Snapshot{}, capture, err
	}
	snapshot, err := s.observe(capture)
	if err != nil {
		return Snapshot{}, capture, err
	}
	if accepted != nil {
		if err := capture.ValidateAcceptedAuthentication(*accepted, view); err != nil {
			return Snapshot{}, capture, err
		}
	}
	return snapshot, capture, ctx.Err()
}

// observe assigns a generation to an already coherent capture. Mutation
// completion passes its exact transaction receipt, never a replacement read.
func (s *Service) observe(capture config.AuthenticationCapture) (Snapshot, error) {
	if s.sequence == 0 || !capture.SameObservation(s.last) {
		if s.sequence == math.MaxUint64 {
			return Snapshot{}, errors.New("authentication generation exhausted; reopen the workspace")
		}
		s.sequence++
		s.last = capture
	}
	snapshot := Snapshot{WorkspaceID: s.workspaceID, Generation: Generation{Epoch: s.epoch, Sequence: s.sequence}, Providers: []Status{}}
	for _, provider := range capture.Providers() {
		slots := []CredentialSlot{}
		for _, slot := range capture.CredentialSlots(provider.Owner) {
			slots = append(slots, CredentialSlot{ID: slot.ID, Kind: slot.Kind, Property: slot.Property, Configured: slot.Configured})
		}
		keyState := "absent"
		if provider.APIKeyConfigured {
			keyState = "configured"
		}
		snapshot.Providers = append(snapshot.Providers, Status{
			Owner: PublicOwner(provider.Owner), Configured: provider.Configured, Disabled: provider.Disabled,
			Credentials:     []CredentialStatus{{Kind: "api-key", State: keyState}, {Kind: "oauth", State: provider.OAuthState, Refreshable: provider.OAuthRefreshable}},
			CredentialSlots: slots,
			AccountState:    provider.AccountState, ActiveAccountID: provider.ActiveAccountID,
		})
	}
	if err := snapshot.Validate(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s *Service) status(ctx context.Context, accepted *config.RemoteRuntimeProposal, view *config.Config) (Snapshot, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	if err := s.acquire(ctx); err != nil {
		return Snapshot{}, err
	}
	defer func() { <-s.gate }()
	snapshot, _, err := s.capture(ctx, accepted, view)
	return snapshot, err
}

func (s *Service) accounts(ctx context.Context, target Target, accepted *config.RemoteRuntimeProposal, view *config.Config) (AccountsState, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	if err := target.Validate(); err != nil {
		return AccountsState{}, err
	}
	if target.WorkspaceID != s.workspaceID {
		return AccountsState{}, ErrStale
	}
	if err := s.acquire(ctx); err != nil {
		return AccountsState{}, err
	}
	defer func() { <-s.gate }()
	// An alien workspace/service incarnation cannot trigger account-file I/O.
	if target.Generation.Epoch != s.epoch {
		return AccountsState{}, ErrStale
	}
	snapshot, capture, err := s.capture(ctx, accepted, view)
	if err != nil {
		return AccountsState{}, err
	}
	if snapshot.Generation != target.Generation {
		return AccountsState{}, ErrStale
	}
	state, err := accountsState(snapshot, capture, target.Owner)
	if err != nil {
		return AccountsState{}, err
	}
	return state, ctx.Err()
}

func accountsState(snapshot Snapshot, capture config.AuthenticationCapture, owner Owner) (AccountsState, error) {
	for i, provider := range capture.Providers() {
		if PublicOwner(provider.Owner) != owner {
			continue
		}
		accounts, err := capture.Accounts(provider.Owner)
		if err != nil {
			return AccountsState{}, ErrOwner
		}
		state := AccountsState{Target: Target{WorkspaceID: snapshot.WorkspaceID, Owner: owner, Generation: snapshot.Generation}, Status: snapshot.Providers[i], Accounts: []AccountSummary{}}
		for _, account := range accounts {
			state.Accounts = append(state.Accounts, AccountSummary{ID: account.ID, DisplayName: account.DisplayName, Active: account.Active, CredentialState: account.CredentialState, Refreshable: account.Refreshable, ExpiresAt: account.ExpiresAt})
		}
		if err := state.Validate(); err != nil {
			return AccountsState{}, err
		}
		return state, nil
	}
	return AccountsState{}, ErrOwner
}
