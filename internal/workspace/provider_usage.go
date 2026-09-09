package workspace

import (
	"context"

	"github.com/example-git/crux/internal/config"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerregistry"
)

func (w *AppWorkspace) PrepareProviderUsage(owner providerregistry.RegistrationOwner) oauthusage.Request {
	return config.PrepareProviderUsage(w.store.Config, owner)
}

func (w *ClientWorkspace) PrepareProviderUsage(owner providerregistry.RegistrationOwner) oauthusage.Request {
	request := config.ProviderUsageRequest{Owner: owner}
	id := w.workspaceID()
	validate := func() error {
		if w.workspaceID() != id {
			return config.ErrProviderUsageAuthority
		}
		return nil
	}
	if authority := w.cached().Authority; authority != nil && authority.Mode == "client" {
		a := w.authority
		if a == nil {
			return func(context.Context) (*oauthusage.Usage, error) { return nil, config.ErrProviderUsageAuthority }
		}
		request.Revision, request.Digest = authority.Revision, authority.Digest
		principal := authority.Principal
		validate = func() error {
			a.mu.Lock()
			defer a.mu.Unlock()
			if w.workspaceID() != id || a.principal != principal || a.accepted.Revision != request.Revision || a.accepted.Digest != request.Digest || a.pending != nil {
				return config.ErrProviderUsageAuthority
			}
			return nil
		}
	}
	return func(ctx context.Context) (*oauthusage.Usage, error) {
		if err := validate(); err != nil {
			return nil, err
		}
		result, err := w.client.ProviderUsage(ctx, id, request)
		if err != nil {
			return nil, err
		}
		if err := validate(); err != nil {
			return nil, err
		}
		return result.Usage, nil
	}
}
