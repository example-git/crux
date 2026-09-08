package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/google/uuid"
)

// ClientRefreshRequest contains identifiers only. The receiver never exchanges
// a client refresh token and a response never contains credential material.
type ClientRefreshRequest struct {
	ID           string                             `json:"id"`
	Principal    string                             `json:"principal"`
	Revision     uint64                             `json:"revision"`
	Digest       string                             `json:"digest"`
	Owner        providerregistry.RegistrationOwner `json:"owner"`
	AccountID    string                             `json:"account_id"`
	CredentialID string                             `json:"credential_id"`
	Deadline     int64                              `json:"deadline"`
}

type ClientRefreshCompletion struct {
	RequestID    string `json:"request_id"`
	Revision     uint64 `json:"revision,omitempty"`
	Digest       string `json:"digest,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	Failed       bool   `json:"failed,omitempty"`
}

type clientRefreshCall struct {
	request   ClientRefreshRequest
	done      chan struct{}
	completed bool
	response  ClientRefreshCompletion
	snapshot  RuntimeSnapshot
	err       error
	waiters   int
}

func (s *ConfigStore) SetClientRefreshPublisher(publish func(context.Context, ClientRefreshRequest)) {
	s.clientRefreshMu.Lock()
	defer s.clientRefreshMu.Unlock()
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
	if err := ctx.Err(); err != nil {
		return RuntimeSnapshot{}, err
	}
	authority := admitted.RemoteAuthority()
	account, ok := admitted.EphemeralAccount(owner)
	if authority == nil || !ok || account.ID == "" {
		return RuntimeSnapshot{}, errors.New("client refresh requires an admitted account runtime")
	}
	credentialID := accounts.CredentialID(*account)
	key := fmt.Sprintf("%s/%d/%s/%s/%#v", authority.Principal, authority.Revision, authority.Digest, credentialID, owner)
	s.clientRefreshMu.Lock()
	call, exists := s.clientRefreshes[key]
	publish := s.clientRefreshPublisher
	if !exists {
		current := s.RemoteAuthority()
		if current == nil || current.Principal != authority.Principal || current.Revision != authority.Revision || current.Digest != authority.Digest {
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
		call = &clientRefreshCall{done: make(chan struct{}), request: ClientRefreshRequest{
			ID: uuid.NewString(), Principal: authority.Principal, Revision: authority.Revision, Digest: authority.Digest,
			Owner: owner, AccountID: account.ID, CredentialID: credentialID, Deadline: time.Now().Add(3 * time.Minute).UnixMilli(),
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
			call.err = errors.New("owning client could not refresh the accepted account; check client authentication")
		} else {
			snapshot := s.RuntimeSnapshot()
			authority := snapshot.RemoteAuthority()
			account, ok := snapshot.EphemeralAccount(call.request.Owner)
			if authority == nil || authority.Principal != principal || authority.Revision != response.Revision || authority.Revision <= call.request.Revision || authority.Digest != response.Digest || !ok || account.ID != call.request.AccountID || accounts.CredentialID(*account) != response.CredentialID || response.CredentialID == call.request.CredentialID {
				return errors.New("client refresh completion does not match an accepted account rotation")
			}
			call.snapshot = snapshot
		}
		call.response, call.completed = response, true
		close(call.done)
		return nil
	}
	return errors.New("client refresh request is not pending")
}
