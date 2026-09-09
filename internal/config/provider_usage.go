package config

import (
	"context"
	"errors"

	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerregistry"
)

var (
	ErrProviderUsageAuthority  = errors.New("provider usage runtime changed")
	ErrProviderUsageOwner      = errors.New("provider usage owner changed")
	ErrProviderUsageCredential = errors.New("provider usage credential changed")
)

// ProviderUsageRequest carries only public selection identity. The authenticated
// workspace supplies the principal; no credential or account lookup is requested.
type ProviderUsageRequest struct {
	Owner    providerregistry.RegistrationOwner `json:"owner"`
	Revision uint64                             `json:"revision"`
	Digest   string                             `json:"digest"`
}

type ProviderUsageResult struct {
	Usage    *oauthusage.Usage `json:"usage"`
	Revision uint64            `json:"revision"`
	Digest   string            `json:"digest"`
}

// PrepareProviderUsage captures the selected local owner and exact configured
// token without I/O. The returned operation checks that selection before and
// after execution, preserving the local UI's account replacement semantics.
func PrepareProviderUsage(current func() *Config, owner providerregistry.RegistrationOwner) oauthusage.Request {
	cfg := current()
	if cfg == nil || cfg.Providers == nil {
		return func(context.Context) (*oauthusage.Usage, error) { return nil, ErrProviderUsageOwner }
	}
	registration, registered := cfg.ProviderRegistration(owner.ProviderID)
	provider, configured := cfg.Providers.Get(owner.ProviderID)
	if !registered || registration.Owner() != owner || !configured || provider.Disable {
		return func(context.Context) (*oauthusage.Usage, error) { return nil, ErrProviderUsageOwner }
	}
	token := providerUsageToken(provider, registration.QuotaCredential)
	validate := func() error {
		cfg := current()
		if cfg == nil || cfg.Providers == nil {
			return ErrProviderUsageOwner
		}
		active, ok := cfg.ProviderRegistration(owner.ProviderID)
		if !ok || active.Owner() != owner || active.QuotaCredential != registration.QuotaCredential {
			return ErrProviderUsageOwner
		}
		provider, ok := cfg.Providers.Get(owner.ProviderID)
		if !ok || provider.Disable || providerUsageToken(provider, active.QuotaCredential) != token {
			return ErrProviderUsageCredential
		}
		return nil
	}
	return func(ctx context.Context) (*oauthusage.Usage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validate(); err != nil {
			return nil, err
		}
		result, err := oauthusage.FetchWithTokenForOwner(ctx, owner.ProviderID, token, registration.Quota, validate)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result, nil
	}
}

func providerUsageToken(provider ProviderConfig, credential providerregistry.QuotaCredential) string {
	if provider.OAuthToken == nil {
		return ""
	}
	switch credential {
	case "", providerregistry.QuotaCredentialAccessToken:
		return provider.OAuthToken.AccessToken
	case providerregistry.QuotaCredentialRefreshToken:
		return provider.OAuthToken.RefreshToken
	default:
		return ""
	}
}

// ProviderUsage admits against one immutable runtime. Client-owned operations
// keep that captured runtime while in flight; the client rejects stale results.
// Server-owned operations retain local current-owner/current-token validation.
func (s *ConfigStore) ProviderUsage(ctx context.Context, request ProviderUsageRequest) (*ProviderUsageResult, error) {
	snapshot := s.RuntimeSnapshot()
	result := &ProviderUsageResult{}
	current := s.Config
	if authority := snapshot.RemoteAuthority(); authority != nil {
		result.Revision, result.Digest = authority.Revision, authority.Digest
		current = snapshot.Config
	}
	if request.Revision != result.Revision || request.Digest != result.Digest {
		return nil, ErrProviderUsageAuthority
	}
	if err := snapshot.ClientProviderUnavailable(request.Owner.ProviderID); err != nil {
		return nil, err
	}
	usage, err := PrepareProviderUsage(current, request.Owner)(ctx)
	if err != nil {
		return nil, err
	}
	result.Usage = usage
	return result, nil
}
