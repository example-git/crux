package providerauth

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
)

// Reload is an explicit owner-local action, separate from mutation recovery.
// A retained receipt is historical; it does not revive its initiating target.
type ReloadRequest struct {
	ReloadID string `json:"reload_id"`
	Target   Target `json:"target"`
}
type ReloadOutcome struct {
	ReloadID string    `json:"reload_id"`
	Previous Target    `json:"previous"`
	Reloaded bool      `json:"reloaded"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

func (r ReloadRequest) Validate() error {
	return (LogoutRequest{OperationID: r.ReloadID, Target: r.Target}).Validate()
}
func (o ReloadOutcome) Validate(r ReloadRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if o.ReloadID != r.ReloadID || o.Previous != r.Target || o.Snapshot != nil && !o.Reloaded {
		return ErrReceiptUnverified
	}
	if o.Snapshot != nil {
		if err := o.Snapshot.Validate(); err != nil {
			return err
		}
		if o.Snapshot.WorkspaceID != r.Target.WorkspaceID || o.Snapshot.Generation.Epoch != r.Target.Generation.Epoch || o.Snapshot.Generation.Sequence <= r.Target.Generation.Sequence {
			return ErrReceiptUnverified
		}
	}
	return nil
}

type reloadReceipt struct {
	request ReloadRequest
	outcome ReloadOutcome
	err     error
}

func cloneReloadOutcome(o ReloadOutcome) ReloadOutcome {
	if o.Snapshot != nil {
		value := *o.Snapshot
		value.Providers = slices.Clone(value.Providers)
		for i := range value.Providers {
			value.Providers[i].Credentials = slices.Clone(value.Providers[i].Credentials)
			value.Providers[i].CredentialSlots = slices.Clone(value.Providers[i].CredentialSlots)
		}
		o.Snapshot = &value
	}
	return o
}

// CaptureSavedAuthentication checks a fresh current-local target and returns
// private proof for review only. Accepted receiver state is deliberately not
// substituted here; saved review is a separate explicit publication intent.
func (s *Service) CaptureSavedAuthentication(ctx context.Context, target Target) (config.AuthenticationCapture, providerregistry.RegistrationOwner, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	var zero config.AuthenticationCapture
	var owner providerregistry.RegistrationOwner
	if err := target.Validate(); err != nil {
		return zero, owner, err
	}
	if target.WorkspaceID != s.workspaceID || target.Generation.Epoch != s.epoch {
		return zero, owner, ErrStale
	}
	if err := s.acquire(ctx); err != nil {
		return zero, owner, err
	}
	defer func() { <-s.gate }()
	snapshot, capture, err := s.capture(ctx, nil, nil)
	if err != nil {
		return zero, owner, err
	}
	if snapshot.Generation != target.Generation {
		return zero, owner, ErrStale
	}
	for _, p := range capture.Providers() {
		if PublicOwner(p.Owner) == target.Owner {
			return capture, p.Owner, nil
		}
	}
	return zero, owner, ErrOwner
}

func (s *Service) ReloadSavedAuthentication(ctx context.Context, request ReloadRequest) (ReloadOutcome, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	initial := ReloadOutcome{ReloadID: request.ReloadID, Previous: request.Target}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	if request.Target.WorkspaceID != s.workspaceID || request.Target.Generation.Epoch != s.epoch {
		return initial, ErrStale
	}
	if err := s.acquire(ctx); err != nil {
		return initial, err
	}
	defer func() { <-s.gate }()
	if receipt, ok := s.reloads[request.ReloadID]; ok {
		if receipt.request != request {
			return initial, ErrOperationConflict
		}
		return cloneReloadOutcome(receipt.outcome), receipt.err
	}
	snapshot, before, err := s.capture(ctx, nil, nil)
	if err != nil {
		return initial, err
	}
	if snapshot.Generation != request.Target.Generation {
		return initial, ErrStale
	}
	var owner providerregistry.RegistrationOwner
	for _, p := range before.Providers() {
		if PublicOwner(p.Owner) == request.Target.Owner {
			owner = p.Owner
			break
		}
	}
	if owner.ProviderID == "" {
		return initial, ErrOwner
	}
	if s.sequence >= math.MaxUint64-1 {
		return initial, errors.New("authentication generation exhausted; reopen the workspace")
	}
	// Consume this generation before any disk load or source evaluation. Keep
	// old checks and mutations as historical receipts, with no current authority.
	s.sequence++
	receipt := reloadReceipt{request: request, outcome: initial}
	after, published, err := s.store.ReloadAuthenticationFromDisk(ctx, before, owner)
	receipt.outcome.Reloaded = published
	if err == nil {
		current, observeErr := s.observe(after)
		err = observeErr
		if err == nil {
			receipt.outcome.Snapshot = &current
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			err = errors.New("saved configuration reload could not complete; inspect saved settings and retry this exact receipt or start a new explicit reload")
		}
	}
	receipt.err = err
	if s.reloads == nil {
		s.reloads = map[string]reloadReceipt{}
	}
	if len(s.reloadIDs) == mutationReceiptLimit {
		delete(s.reloads, s.reloadIDs[0])
		s.reloadIDs = s.reloadIDs[1:]
	}
	s.reloadIDs = append(s.reloadIDs, request.ReloadID)
	s.reloads[request.ReloadID] = receipt
	return cloneReloadOutcome(receipt.outcome), err
}
