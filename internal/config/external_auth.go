package config

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
)

type ExternalAuthenticationRequest struct {
	Service      string `json:"service"`
	IncludeToken bool   `json:"include_token,omitempty"`
	RefreshToken bool   `json:"refresh_token,omitempty"`
}

type ExternalAuthentication struct {
	Required bool   `json:"required"`
	Method   string `json:"method,omitempty"`
	Token    string `json:"token,omitempty"`
}

func (ExternalAuthentication) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private external authentication]"))
}

var externalAuthServicePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func (request ExternalAuthenticationRequest) Validate() error {
	if !externalAuthServicePattern.MatchString(request.Service) {
		return errors.New("external authentication service is invalid")
	}
	return nil
}

func (s *ConfigStore) ExternalAuthentication(ctx context.Context, request ExternalAuthenticationRequest) (ExternalAuthentication, error) {
	if err := request.Validate(); err != nil {
		return ExternalAuthentication{}, err
	}
	var admittedOwner providerregistry.RegistrationOwner
	var admittedPolicy manifest.ExternalAuthPolicy
	var admittedSelection SelectedModel
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return ExternalAuthentication{}, err
		}
		snapshot := s.RuntimeSnapshot()
		if snapshot.IsClientOwned() {
			return ExternalAuthentication{}, ErrClientRuntimeManaged
		}
		cfg := snapshot.Config()
		if cfg == nil {
			return ExternalAuthentication{}, errors.New("selected provider configuration is unavailable")
		}
		selection := cfg.Models[SelectedModelTypeLarge]
		provider, configured := cfg.authenticationCollectionProvider(selection.Provider)
		owner, available := snapshot.ProviderOwnerFor(selection.Provider, provider)
		if !configured || !available || provider.Disable {
			return ExternalAuthentication{}, errors.New("selected provider owner is unavailable")
		}
		registration, available := snapshot.ProviderBehaviorRegistration(selection.Provider, provider)
		if !available && provider.Plugin != nil {
			return ExternalAuthentication{}, errors.New("selected provider behavior is unavailable")
		}
		var policy *manifest.ExternalAuthPolicy
		if registration.Manifest != nil {
			policy = registration.Manifest.Capabilities.ExternalAuth
		}
		if attempt != 0 && (owner != admittedOwner || selection.Provider != admittedSelection.Provider || selection.Model != admittedSelection.Model || policy == nil || *policy != admittedPolicy) {
			return ExternalAuthentication{}, errors.New("selected provider changed during authentication refresh")
		}
		if policy == nil || policy.Service != request.Service {
			return ExternalAuthentication{}, nil
		}
		admittedOwner, admittedPolicy, admittedSelection = owner, *policy, selection
		var credential manifest.Credential
		for _, declared := range registration.Manifest.Capabilities.Credentials {
			if declared.ID == policy.Credential {
				credential = declared
				break
			}
		}
		result := ExternalAuthentication{Required: true}
		var token *oauth.Token
		var entry *accounts.Entry
		var capture *AuthenticationCapture
		var value string
		switch credential.Kind {
		case "oauth2":
			if owner.AccountNamespace != "" {
				state, err := s.CaptureAuthentication(ctx)
				if err != nil {
					return ExternalAuthentication{}, err
				}
				if !snapshot.SamePublication(state.runtime) {
					return ExternalAuthentication{}, errors.New("selected provider changed during authentication capture")
				}
				capture = &state
				active := state.accounts.ActiveID(owner.AccountNamespace)
				for _, candidate := range state.accounts.Entries(owner.AccountNamespace) {
					if candidate.ID == active && active != "" {
						entry = &candidate
						break
					}
				}
				if entry != nil {
					if !providerHasAccount(provider, *entry) {
						return ExternalAuthentication{}, errors.New("selected provider and active account are out of sync")
					}
					token = entry.Token()
				} else if provider.APIKey != "" || provider.OAuthToken != nil {
					return ExternalAuthentication{}, errors.New("selected provider has no matching active account")
				}
			} else {
				token = provider.OAuthToken
				if token != nil && provider.APIKey != token.AccessToken {
					return ExternalAuthentication{}, errors.New("selected provider OAuth credential is out of sync")
				}
			}
			if token != nil {
				if attempt == 0 && token.RefreshToken != "" && (request.RefreshToken || token.IsExpired()) {
					var err error
					if entry != nil {
						_, err = s.RefreshSelectedOAuthAccountForRuntime(ctx, ScopeGlobal, owner, *entry, true, snapshot)
					} else {
						_, err = s.RefreshProviderOAuthTokenAtOrigin(ctx, owner, token, snapshot)
					}
					if err != nil {
						if ctx.Err() != nil {
							return ExternalAuthentication{}, ctx.Err()
						}
						return ExternalAuthentication{}, errors.New("selected provider authentication refresh failed")
					}
					continue
				}
				if !token.IsExpired() {
					value = token.AccessToken
				}
			}
		case "api-key", "bearer":
			var err error
			if credential.ConfigProperty != "" {
				err = snapshot.validateResolvedConfigurationCredentials(provider)
				if err == nil {
					value, _ = provider.Configuration[credential.ConfigProperty].(string)
					if provider.resolvedCredentials["configuration."+credential.ID] == nil {
						value, err = snapshot.Resolve(value)
					}
				}
			} else {
				value, err = snapshot.ResolveProviderAPIKey(provider)
			}
			if err != nil {
				return ExternalAuthentication{}, errors.New("selected provider authentication credential cannot be resolved")
			}
		default:
			return ExternalAuthentication{}, errors.New("external authentication credential is unsupported")
		}
		if capture != nil {
			current, err := s.CaptureAuthentication(ctx)
			if err != nil {
				return ExternalAuthentication{}, err
			}
			if !capture.SameObservation(current) {
				return ExternalAuthentication{}, errors.New("selected authentication changed during the request")
			}
		} else if !snapshot.SamePublication(s.RuntimeSnapshot()) {
			return ExternalAuthentication{}, errors.New("selected provider changed during authentication")
		}
		if value != "" {
			result.Method = policy.Method
			if request.IncludeToken && policy.TokenExport == "access-token" {
				result.Token = value
			}
		}
		return result, ctx.Err()
	}
	return ExternalAuthentication{}, errors.New("selected authentication refresh did not complete")
}
