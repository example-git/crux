package workspace

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
)

type clientRefreshEvent struct{ deadline int64 }

// HandleClientRefreshEvent is shared by the UI subscription and headless run
// loop. Work runs asynchronously so token exchange cannot stall channel draining.
func (w *ClientWorkspace) HandleClientRefreshEvent(ctx context.Context, event any) bool {
	var request config.ClientRefreshRequest
	switch value := event.(type) {
	case pubsub.Event[config.ClientRefreshRequest]:
		request = value.Payload
	case proto.PeerProviderRefreshRequest:
		var ok bool
		request, ok = w.resolvePeerRefreshRequest(value)
		if !ok {
			return true
		}
	default:
		return false
	}
	a := w.authority
	if a == nil || request.ID == "" || request.Principal != a.principal || request.Deadline <= time.Now().UnixMilli() {
		return true
	}
	if _, exists := a.refreshEvents.LoadOrStore(request.ID, clientRefreshEvent{deadline: request.Deadline}); exists {
		return true
	}
	a.refreshEvents.Range(func(key, value any) bool {
		if value.(clientRefreshEvent).deadline < time.Now().UnixMilli() {
			a.refreshEvents.Delete(key)
		}
		return true
	})
	go func() {
		deadline := min(request.Deadline, time.Now().Add(3*time.Minute).UnixMilli())
		refreshCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(deadline))
		defer cancel()
		workspaceID := w.workspaceID()
		response, err := w.fulfillClientRefresh(refreshCtx, request)
		if err != nil {
			slog.Error("Owning client refresh failed", "provider", request.Owner.ProviderID, "error", err)
			response = config.ClientRefreshCompletion{RequestID: request.ID, Failed: true}
		}
		// Re-send the identical completion after a lost response. The receiver
		// records it by request ID; this never repeats an exchange or runtime PUT.
		retry := time.NewTicker(time.Second)
		defer retry.Stop()
		for {
			err := w.client.CompleteClientRefresh(refreshCtx, workspaceID, response)
			if err == nil {
				return
			}
			if errors.Is(err, client.ErrClientRefreshRejected) {
				slog.Error("Owning client refresh completion rejected", "provider", request.Owner.ProviderID, "error", err)
				if response.Failed {
					return
				}
				// A concurrent accepted revision may no longer satisfy the
				// initiating definition/account. End that old request explicitly;
				// never leave it waiting after this client has stopped retrying.
				response = config.ClientRefreshCompletion{RequestID: request.ID, Failed: true}
				continue
			}
			select {
			case <-refreshCtx.Done():
				return
			case <-retry.C:
			}
		}
	}()
	return true
}

func (w *ClientWorkspace) resolvePeerRefreshRequest(received proto.PeerProviderRefreshRequest) (config.ClientRefreshRequest, bool) {
	a := w.authority
	if a == nil {
		return config.ClientRefreshRequest{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if received.Revision != a.accepted.Revision || received.Digest != a.accepted.Digest {
		return config.ClientRefreshRequest{}, false
	}
	definitionMatched := false
	for _, definition := range a.accepted.Providers {
		if definition.Config.ID != received.Provider.Owner.ProviderID || definition.BundleDigest != received.Provider.BundleDigest {
			continue
		}
		digest, err := definition.Digest()
		if err != nil || digest != received.Provider.DefinitionDigest {
			continue
		}
		definitionMatched = true
		break
	}
	if !definitionMatched {
		return config.ClientRefreshRequest{}, false
	}
	var matched *config.RemoteCredentialBinding
	for i := range a.accepted.Credentials {
		binding := &a.accepted.Credentials[i]
		if binding.Unavailable || providerauth.PublicOwner(binding.Owner) != received.Provider.Owner {
			continue
		}
		if matched != nil {
			return config.ClientRefreshRequest{}, false
		}
		matched = binding
	}
	if matched == nil {
		return config.ClientRefreshRequest{}, false
	}
	request := config.ClientRefreshRequest{
		ID: received.RequestID, Principal: a.principal, Revision: received.Revision, Digest: received.Digest,
		Owner: matched.Owner, DefinitionDigest: received.Provider.DefinitionDigest, BundleDigest: received.Provider.BundleDigest, Deadline: received.Deadline,
	}
	if matched.Account != nil {
		request.AccountID = matched.Account.ID
		request.CredentialID = accounts.CredentialID(*matched.Account)
	} else if matched.OAuthToken != nil {
		request.CredentialID = config.OAuthTokenCredentialID(matched.OAuthToken)
	} else {
		return config.ClientRefreshRequest{}, false
	}
	return request, true
}

func (w *ClientWorkspace) fulfillClientRefresh(ctx context.Context, request config.ClientRefreshRequest) (config.ClientRefreshCompletion, error) {
	a := w.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := w.reconcileClientAuthority(ctx, a); err != nil {
		return config.ClientRefreshCompletion{}, err
	}
	if err := a.requireAuthenticationPublication(w.workspaceID()); err != nil {
		return config.ClientRefreshCompletion{}, err
	}
	if request.Principal != a.principal || request.Revision != a.accepted.Revision || request.Digest != a.accepted.Digest {
		return config.ClientRefreshCompletion{}, errors.New("refresh request does not match this client's accepted runtime")
	}
	var expected *accounts.Entry
	var expectedToken *oauth.Token
	for _, binding := range a.accepted.Credentials {
		if binding.Owner != request.Owner || binding.Unavailable {
			continue
		}
		if binding.Account != nil && binding.Account.ID == request.AccountID && accounts.CredentialID(*binding.Account) == request.CredentialID {
			expected = binding.Account
			break
		}
		if request.Owner.AccountNamespace == "" && request.AccountID == "" && binding.OAuthToken != nil && config.OAuthTokenCredentialID(binding.OAuthToken) == request.CredentialID {
			expectedToken = binding.OAuthToken
			break
		}
	}
	if expected == nil && expectedToken == nil {
		return config.ClientRefreshCompletion{}, errors.New("refresh request does not match the accepted OAuth credential")
	}
	localRuntime, err := a.runtimeForRefresh(request.Owner)
	if err != nil {
		return config.ClientRefreshCompletion{}, err
	}
	var credentialID string
	if expected != nil {
		fresh, err := a.store.RefreshSelectedOAuthAccountForRuntime(ctx, config.ScopeGlobal, request.Owner, *expected, true, localRuntime)
		if err != nil {
			return config.ClientRefreshCompletion{}, err
		}
		credentialID = accounts.CredentialID(*fresh)
	} else {
		fresh, err := a.store.RefreshProviderOAuthTokenAtOrigin(ctx, request.Owner, expectedToken, localRuntime)
		if err != nil {
			return config.ClientRefreshCompletion{}, err
		}
		credentialID = config.OAuthTokenCredentialID(fresh)
	}
	if err := w.publishClientAuthorityLocked(ctx, a); err != nil {
		return config.ClientRefreshCompletion{}, err
	}
	return config.ClientRefreshCompletion{RequestID: request.ID, Revision: a.accepted.Revision, Digest: a.accepted.Digest, CredentialID: credentialID}, nil
}

// The caller holds a.mu so the accepted definition cannot move between this
// comparison and the local account transaction.
func (a *clientAuthority) runtimeForRefresh(expectedOwner providerregistry.RegistrationOwner) (config.RuntimeSnapshot, error) {
	localRuntime := a.store.RuntimeSnapshot()
	definition, owner, err := localRuntime.ClientProviderDefinition(expectedOwner.ProviderID)
	if err != nil {
		return config.RuntimeSnapshot{}, err
	}
	currentDigest, err := definition.Digest()
	if err != nil {
		return config.RuntimeSnapshot{}, err
	}
	acceptedDigest, err := a.accepted.ProviderDefinitionDigest(expectedOwner.ProviderID)
	if err != nil || owner != expectedOwner || currentDigest != acceptedDigest {
		return config.RuntimeSnapshot{}, errors.New("provider definition changed on the owning client; publish its selection before refreshing")
	}
	return localRuntime, nil
}
