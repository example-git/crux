package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/example-git/crux/internal/providerregistry"
	"github.com/google/uuid"
)

// ClientRefreshRequest contains identifiers only. The receiver never exchanges
// a client refresh token and a response never contains credential material.
type ClientRefreshRequest struct {
	ID               string                             `json:"id"`
	Principal        string                             `json:"principal"`
	Revision         uint64                             `json:"revision"`
	Digest           string                             `json:"digest"`
	Owner            providerregistry.RegistrationOwner `json:"owner"`
	DefinitionDigest string                             `json:"definition_digest"`
	BundleDigest     string                             `json:"bundle_digest,omitempty"`
	AccountID        string                             `json:"account_id"`
	CredentialID     string                             `json:"credential_id"`
	Deadline         int64                              `json:"deadline"`
}

type ClientRefreshCompletion struct {
	RequestID    string `json:"request_id"`
	Revision     uint64 `json:"revision,omitempty"`
	Digest       string `json:"digest,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	Failed       bool   `json:"failed,omitempty"`
	// Reason is a non-secret, human-readable explanation of why the owning
	// client reported Failed. It must never contain tokens or other
	// credential material and is only meaningful when Failed is true.
	Reason string `json:"reason,omitempty"`
}

type clientRefreshCall struct {
	request          ClientRefreshRequest
	admittedRevision uint64
	admittedDigest   string
	providerDigest   string
	done             chan struct{}
	completed        bool
	response         ClientRefreshCompletion
	snapshot         RuntimeSnapshot
	err              error
	waiters          int
}

func (s *ConfigStore) SetClientRefreshPublisher(publish func(context.Context, ClientRefreshRequest)) {
	s.clientRefreshMu.Lock()
	defer s.clientRefreshMu.Unlock()
	if s.RuntimeRevocation() != nil {
		return
	}
	s.clientRefreshPublisher = publish
}

func (s *ConfigStore) PendingClientRefreshes() []ClientRefreshRequest {
	s.clientRefreshMu.Lock()
	defer s.clientRefreshMu.Unlock()
	var result []ClientRefreshRequest
	for _, call := range s.clientRefreshes {
		if !call.completed && time.Now().UnixMilli() < call.request.Deadline {
			result = append(result, call.request)
		}
	}
	return result
}

// RequestClientRefresh waits for the owning client to persist a rotation and
// acknowledge an accepted runtime. Identical admitted requests share one call.
// Completed results retain their captured snapshot for concurrent old callers.
func (s *ConfigStore) RequestClientRefresh(ctx context.Context, admitted RuntimeSnapshot, owner providerregistry.RegistrationOwner) (RuntimeSnapshot, error) {
	if err := s.RuntimeRevocation(); err != nil {
		return RuntimeSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return RuntimeSnapshot{}, err
	}
	authority := admitted.RemoteAuthority()
	accountID, credentialID, ok := admitted.clientRefreshCredential(owner)
	if authority == nil || !ok {
		return RuntimeSnapshot{}, errors.New("client refresh requires an admitted OAuth credential runtime")
	}
	providerDigest, err := admitted.clientRuntime.proposal.ProviderDefinitionDigest(owner.ProviderID)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	key := fmt.Sprintf("%s/%d/%s/%s/%#v", authority.Principal, authority.Revision, authority.Digest, credentialID, owner)
	s.clientRefreshMu.Lock()
	if err := s.RuntimeRevocation(); err != nil {
		s.clientRefreshMu.Unlock()
		return RuntimeSnapshot{}, err
	}
	call, exists := s.clientRefreshes[key]
	publish := s.clientRefreshPublisher
	if !exists {
		current := s.RemoteAuthority()
		requestAuthority := authority
		if current != nil && current.Principal == authority.Principal && (current.Revision != authority.Revision || current.Digest != authority.Digest) {
			// An earlier exact refresh from this captured runtime may have
			// advanced a different provider. Only a retained completion receipt
			// can bridge that revision; unrelated runtime updates cannot.
			for _, prior := range s.clientRefreshes {
				if !prior.completed || prior.err != nil || prior.request.Principal != authority.Principal || prior.admittedRevision != authority.Revision || prior.admittedDigest != authority.Digest {
					continue
				}
				accepted := prior.snapshot.RemoteAuthority()
				if accepted == nil || accepted.Revision != current.Revision || accepted.Digest != current.Digest {
					continue
				}
				candidateAccount, candidateCredential, ok := prior.snapshot.clientRefreshCredential(owner)
				candidateDefinition, err := prior.snapshot.clientRuntime.proposal.ProviderDefinitionDigest(owner.ProviderID)
				if ok && err == nil && candidateAccount == accountID && candidateCredential == credentialID && candidateDefinition == providerDigest {
					requestAuthority = current
					break
				}
			}
		}
		if current == nil || current.Principal != requestAuthority.Principal || current.Revision != requestAuthority.Revision || current.Digest != requestAuthority.Digest {
			s.clientRefreshMu.Unlock()
			return RuntimeSnapshot{}, errors.New("admitted client runtime changed before refresh")
		}
		if publish == nil {
			s.clientRefreshMu.Unlock()
			return RuntimeSnapshot{}, errors.New("owning client refresh transport is unavailable")
		}
		if s.clientRefreshes == nil {
			s.clientRefreshes = map[string]*clientRefreshCall{}
		}
		// Requests are bounded per workspace. Discard completed receipts before
		// refusing new concurrent work; existing waiters hold their own pointer.
		for id, previous := range s.clientRefreshes {
			if previous.completed && len(s.clientRefreshes) >= 32 || previous.request.Deadline < time.Now().UnixMilli() {
				delete(s.clientRefreshes, id)
			}
		}
		if len(s.clientRefreshes) >= 64 {
			s.clientRefreshMu.Unlock()
			return RuntimeSnapshot{}, errors.New("too many pending client refresh requests")
		}
		var bundleDigest string
		for _, definition := range admitted.clientRuntime.proposal.Providers {
			if definition.Config.ID == owner.ProviderID {
				bundleDigest = definition.BundleDigest
				break
			}
		}
		call = &clientRefreshCall{done: make(chan struct{}), admittedRevision: authority.Revision, admittedDigest: authority.Digest, providerDigest: providerDigest, request: ClientRefreshRequest{
			ID: uuid.NewString(), Principal: requestAuthority.Principal, Revision: requestAuthority.Revision, Digest: requestAuthority.Digest,
			Owner: owner, DefinitionDigest: providerDigest, BundleDigest: bundleDigest, AccountID: accountID, CredentialID: credentialID, Deadline: time.Now().Add(3 * time.Minute).UnixMilli(),
		}}
		s.clientRefreshes[key] = call
	}
	call.waiters++
	s.clientRefreshMu.Unlock()
	defer func() {
		s.clientRefreshMu.Lock()
		defer s.clientRefreshMu.Unlock()
		call.waiters--
		if call.waiters == 0 && !call.completed && s.clientRefreshes[key] == call {
			delete(s.clientRefreshes, key)
		}
	}()
	if !exists {
		publish(ctx, call.request)
	}
	waitCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(call.request.Deadline))
	defer cancel()
	replay := time.NewTicker(5 * time.Second)
	defer replay.Stop()
	for {
		select {
		case <-call.done:
			if err := s.RuntimeRevocation(); err != nil {
				return RuntimeSnapshot{}, err
			}
			return call.snapshot, call.err
		case <-waitCtx.Done():
			return RuntimeSnapshot{}, fmt.Errorf("waiting for owning client refresh: %w", waitCtx.Err())
		case <-replay.C:
			if publish != nil {
				publish(waitCtx, call.request)
			}
		}
	}
}

// CompleteClientRefresh accepts only an exact principal/request and an already
// published revision containing the same account. Capturing here prevents a
// later account/model change from becoming the waiting operation's retry.
func (s *ConfigStore) CompleteClientRefresh(principal string, response ClientRefreshCompletion) error {
	s.clientRefreshMu.Lock()
	defer s.clientRefreshMu.Unlock()
	if err := s.RuntimeRevocation(); err != nil {
		return err
	}
	for _, call := range s.clientRefreshes {
		if call.request.ID != response.RequestID {
			continue
		}
		if principal != call.request.Principal {
			return errors.New("client refresh principal mismatch")
		}
		if call.completed {
			if call.response != response {
				return errors.New("client refresh completion differs from its accepted result")
			}
			return nil
		}
		if time.Now().UnixMilli() >= call.request.Deadline {
			return errors.New("client refresh request expired")
		}
		if response.Failed {
			if response.Reason != "" {
				call.err = fmt.Errorf("owning client could not refresh the accepted account: %s", response.Reason)
			} else {
				call.err = errors.New("owning client could not refresh the accepted account; check client authentication")
			}
		} else {
			// Match against the store's current accepted state rather than
			// requiring authority.Revision/Digest to equal the values the
			// client echoed. The client computes those values while holding
			// its own authority lock, then releases it before this
			// completion is sent; an unrelated concurrent publish from the
			// same principal (for example a model switch that lands right
			// after this rotation) can legitimately advance the accepted
			// revision again in that gap while still carrying the exact
			// rotated credential forward unchanged. Requiring byte-for-byte
			// equality there would reject an otherwise-successful refresh
			// and discard a credential rotation that already happened. The
			// account, credential, and provider-definition checks below
			// still verify this exact rotation is what the current accepted
			// state actually reflects, so an unrelated account/credential
			// change is still rejected.
			snapshot := s.RuntimeSnapshot()
			authority := snapshot.RemoteAuthority()
			accountID, credentialID, ok := snapshot.clientRefreshCredential(call.request.Owner)
			if authority == nil || authority.Principal != principal || authority.Revision <= call.request.Revision || !ok || accountID != call.request.AccountID || credentialID != response.CredentialID || response.CredentialID == call.request.CredentialID {
				return errors.New("client refresh completion does not match an accepted account rotation")
			}
			digest, err := snapshot.clientRuntime.proposal.ProviderDefinitionDigest(call.request.Owner.ProviderID)
			if err != nil || digest != call.providerDigest {
				return errors.New("provider definition changed during client refresh; the new runtime remains accepted")
			}
			call.snapshot = snapshot
		}
		call.response, call.completed = response, true
		close(call.done)
		return nil
	}
	return errors.New("client refresh request is not pending")
}
