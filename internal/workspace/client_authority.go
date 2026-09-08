package workspace

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
)

// CredentialEditor completes account/config changes before a remote snapshot
// is collected. Callers never mutate a receiver's persistent configuration.
type CredentialEditor interface {
	SetProviderAPIKey(config.Scope, string, any) error
	RemoveProviderCredentials(config.Scope, providerregistry.RegistrationOwner) error
}

// TransactCredentials preserves the existing local/server-owned behavior and
// batches owning-client account writes into one acknowledged remote update.
func TransactCredentials(ctx context.Context, w Workspace, mutate func(CredentialEditor) error) error {
	if remote, ok := w.(*ClientWorkspace); ok && remote.clientOwned() {
		return remote.transactClientCredentials(ctx, mutate)
	}
	return mutate(w)
}

type clientAuthority struct {
	mu            sync.Mutex
	store         *config.ConfigStore
	view          atomic.Pointer[config.Config]
	accepted      config.RemoteRuntimeProposal
	principal     string
	creation      proto.Workspace
	pending       *config.RemoteRuntimeProposal
	pendingView   *config.Config
	removed       map[providerregistry.RegistrationOwner]bool
	refreshEvents sync.Map
}

func newClientAuthority(c *client.Client, ws proto.Workspace) *clientAuthority {
	if c == nil || c.LocalRuntimeStore() == nil || ws.Authority == nil || ws.Authority.Mode != "client" || ws.Runtime == nil || ws.Creation == nil {
		return nil
	}
	a := &clientAuthority{store: c.LocalRuntimeStore(), accepted: *ws.Runtime, principal: ws.Authority.Principal, creation: *ws.Creation, removed: map[providerregistry.RegistrationOwner]bool{}}
	a.view.Store(c.LocalRuntimeStore().Config())
	for _, credential := range a.accepted.Credentials {
		providerDisabled := false
		for _, definition := range a.accepted.Providers {
			if definition.Config.ID == credential.Owner.ProviderID {
				providerDisabled = definition.Config.Disable
			}
		}
		if credential.Unavailable && !providerDisabled {
			a.removed[credential.Owner] = true
		}
	}
	return a
}

func (a *clientAuthority) configView() *config.Config {
	return a.view.Load()
}

func (w *ClientWorkspace) clientOwned() bool {
	a := w.cached().Authority
	return a != nil && a.Mode == "client"
}

func matchesAuthority(ack *config.RemoteAuthority, principal string, proposal config.RemoteRuntimeProposal) bool {
	return ack != nil && ack.Mode == "client" && ack.Principal == principal && ack.Revision == proposal.Revision && ack.Digest == proposal.Digest
}

// reconcileClientAuthority handles an ambiguous previous PUT before another
// mutation. It never overwrites a different client's accepted revision.
func (w *ClientWorkspace) reconcileClientAuthority(ctx context.Context, a *clientAuthority) error {
	if a.pending == nil {
		return nil
	}
	remote, err := w.client.GetWorkspace(ctx, w.workspaceID())
	if err != nil {
		return fmt.Errorf("cannot reconcile pending client runtime: %w", err)
	}
	if matchesAuthority(remote.Authority, a.principal, *a.pending) {
		a.accepted = *a.pending
		a.view.Store(a.pendingView)
		a.pending, a.pendingView = nil, nil
		w.adoptRuntimeResponse(*remote)
		return nil
	}
	if matchesAuthority(remote.Authority, a.principal, a.accepted) {
		a.pending, a.pendingView = nil, nil
		return nil
	}
	return errors.New("remote runtime changed outside this client transaction; reconnect to explicitly publish current client state")
}

func (w *ClientWorkspace) adoptRuntimeResponse(updated proto.Workspace) {
	if updated.Config != nil {
		updated.Config.SetupAgents()
	}
	sequence := w.refreshSequence.Add(1)
	w.mu.Lock()
	defer w.mu.Unlock()
	if updated.ID == w.ws.ID && !olderClientAuthority(updated.Authority, w.ws.Authority) {
		w.ws, w.appliedRefresh = updated, sequence
	}
}

func (w *ClientWorkspace) mutateClientAuthority(ctx context.Context, mutate func(*config.ConfigStore) error) error {
	a := w.authority
	if a == nil {
		return errors.New("owning client configuration is unavailable; reconnect with a collected client runtime")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := w.reconcileClientAuthority(ctx, a); err != nil {
		return err
	}
	if err := mutate(a.store); err != nil {
		return err
	}
	return w.publishClientAuthorityLocked(ctx, a)
}

func (w *ClientWorkspace) mutateClientPresentation(mutate func(*config.ConfigStore) error) error {
	a := w.authority
	if a == nil {
		return errors.New("owning client configuration is unavailable")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := mutate(a.store); err != nil {
		return err
	}
	local := a.store.Config()
	a.view.Store(a.configView().WithClientPresentation(local))
	if a.pendingView != nil {
		a.pendingView = a.pendingView.WithClientPresentation(local)
	}
	return nil
}

func (w *ClientWorkspace) publishClientAuthorityLocked(ctx context.Context, a *clientAuthority) error {
	proposal, err := a.store.CollectRemoteRuntimeWithUnavailable(ctx, a.accepted.Revision+1, a.removed)
	if err != nil {
		return fmt.Errorf("client state saved; remote runtime was not updated: %w", err)
	}
	a.pending, a.pendingView = &proposal, a.store.Config()
	ack, err := w.client.ReplaceRemoteRuntime(ctx, w.workspaceID(), a.accepted.Revision, proposal)
	if err != nil {
		// A committed request may lose its response. Only its exact content
		// acknowledgement can turn that ambiguity into success.
		if remote, lookupErr := w.client.GetWorkspace(ctx, w.workspaceID()); lookupErr == nil && matchesAuthority(remote.Authority, a.principal, proposal) {
			ack, err = remote.Authority, nil
			w.adoptRuntimeResponse(*remote)
		}
	}
	if err != nil {
		return fmt.Errorf("client state saved; remote runtime acknowledgement is pending: %w", err)
	}
	if !matchesAuthority(ack, a.principal, proposal) {
		return errors.New("client state saved; remote runtime acknowledgement has a different identity")
	}
	a.accepted = proposal
	a.view.Store(a.pendingView)
	a.pending, a.pendingView = nil, nil
	w.mu.Lock()
	w.ws.Authority = ack
	w.appliedRefresh = w.refreshSequence.Add(1)
	w.mu.Unlock()
	return nil
}

type clientCredentialEditor struct{ a *clientAuthority }

func (e clientCredentialEditor) SetProviderAPIKey(scope config.Scope, id string, value any) error {
	if err := e.a.store.SetProviderAPIKey(scope, id, value); err != nil {
		return err
	}
	if owner, ok := e.a.store.RuntimeSnapshot().ProviderOwner(id); ok {
		delete(e.a.removed, owner)
	}
	return nil
}

func (e clientCredentialEditor) RemoveProviderCredentials(scope config.Scope, owner providerregistry.RegistrationOwner) error {
	if err := e.a.store.RemoveProviderCredentials(scope, owner); err != nil {
		return err
	}
	e.a.removed[owner] = true
	return nil
}

func (w *ClientWorkspace) transactClientCredentials(ctx context.Context, mutate func(CredentialEditor) error) error {
	return w.mutateClientAuthority(ctx, func(*config.ConfigStore) error { return mutate(clientCredentialEditor{w.authority}) })
}

func olderClientAuthority(incoming, current *config.RemoteAuthority) bool {
	if current == nil || current.Mode != "client" {
		return false
	}
	return incoming == nil || incoming.Mode != "client" || incoming.Principal != current.Principal || incoming.Revision < current.Revision
}

// recreateClientWorkspace recollects authority independently of public state.
// Holding the authority mutex through ID adoption prevents a configuration
// transaction from publishing to the lost ID while recreation is in flight.
func (w *ClientWorkspace) recreateClientWorkspace(ctx context.Context) (*proto.Workspace, error) {
	a := w.authority
	if a == nil {
		return nil, errors.New("owning client authority is unavailable for recovery")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := w.client.NegotiateRemoteRuntime(ctx); err != nil {
		return nil, err
	}
	proposal, err := a.store.CollectRemoteRuntimeWithUnavailable(ctx, a.accepted.Revision, a.removed)
	if err != nil {
		return nil, fmt.Errorf("recollect client runtime for recovery: %w", err)
	}
	if proposal.Digest != a.accepted.Digest {
		if a.accepted.Revision == ^uint64(0) {
			return nil, errors.New("client runtime revision exhausted")
		}
		proposal, err = a.store.CollectRemoteRuntimeWithUnavailable(ctx, a.accepted.Revision+1, a.removed)
		if err != nil {
			return nil, err
		}
	}
	request := a.creation
	request.AuthorityMode, request.Runtime = "client", &proposal
	created, err := w.client.CreateWorkspace(ctx, request)
	if err != nil {
		return nil, err
	}
	a.accepted = proposal
	a.view.Store(a.store.Config())
	a.pending, a.pendingView = nil, nil
	w.installRecoveredWorkspace(*created)
	return created, nil
}
