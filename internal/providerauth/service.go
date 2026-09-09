package providerauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"

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
}

func New(store *config.ConfigStore, workspaceID string) *Service {
	var epoch [16]byte
	// crypto/rand.Read either fills the buffer or terminates the process on a
	// broken OS entropy source. No predictable authentication incarnation fallback.
	_, _ = rand.Read(epoch[:])
	return &Service{store: store, workspaceID: workspaceID, epoch: hex.EncodeToString(epoch[:]), gate: make(chan struct{}, 1)}
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
	case <-ctx.Done():
		return ctx.Err()
	case s.gate <- struct{}{}:
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
	if s.sequence == 0 || !capture.SameObservation(s.last) {
		if s.sequence == math.MaxUint64 {
			return Snapshot{}, capture, errors.New("authentication generation exhausted; reopen the workspace")
		}
		s.sequence++
		s.last = capture
	}
	if accepted != nil {
		if err := capture.ValidateAcceptedAuthentication(*accepted, view); err != nil {
			return Snapshot{}, capture, err
		}
	}
	snapshot := Snapshot{WorkspaceID: s.workspaceID, Generation: Generation{Epoch: s.epoch, Sequence: s.sequence}, Providers: []Status{}}
	for _, provider := range capture.Providers() {
		keyState := "absent"
		if provider.APIKeyConfigured {
			keyState = "configured"
		}
		snapshot.Providers = append(snapshot.Providers, Status{
			Owner: PublicOwner(provider.Owner), Configured: provider.Configured, Disabled: provider.Disabled,
			Credentials:  []CredentialStatus{{Kind: "api-key", State: keyState}, {Kind: "oauth", State: provider.OAuthState, Refreshable: provider.OAuthRefreshable}},
			AccountState: provider.AccountState, ActiveAccountID: provider.ActiveAccountID,
		})
	}
	if err := snapshot.Validate(); err != nil {
		return Snapshot{}, capture, err
	}
	return snapshot, capture, ctx.Err()
}

func (s *Service) status(ctx context.Context, accepted *config.RemoteRuntimeProposal, view *config.Config) (Snapshot, error) {
	if err := s.acquire(ctx); err != nil {
		return Snapshot{}, err
	}
	defer func() { <-s.gate }()
	snapshot, _, err := s.capture(ctx, accepted, view)
	return snapshot, err
}

func (s *Service) accounts(ctx context.Context, target Target, accepted *config.RemoteRuntimeProposal, view *config.Config) (AccountsState, error) {
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
	for i, provider := range capture.Providers() {
		if PublicOwner(provider.Owner) != target.Owner {
			continue
		}
		accounts, err := capture.Accounts(provider.Owner)
		if err != nil {
			return AccountsState{}, ErrOwner
		}
		state := AccountsState{Target: target, Status: snapshot.Providers[i], Accounts: []AccountSummary{}}
		for _, account := range accounts {
			state.Accounts = append(state.Accounts, AccountSummary{ID: account.ID, DisplayName: account.DisplayName, Active: account.Active, CredentialState: account.CredentialState, Refreshable: account.Refreshable})
		}
		if err := state.Validate(); err != nil {
			return AccountsState{}, err
		}
		return state, ctx.Err()
	}
	return AccountsState{}, ErrOwner
}
