package config

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
)

// DurableObservationID is private equality evidence for a later explicit
// recovery. It does not recreate a store publication or prove any HTTP result.
// Unlike SameObservation it survives a process restart, but requires all saved
// inputs, accepted source/literal bindings, owners and captured environment to
// agree. A changed file identity, shell result or account metadata is a conflict.
func (c AuthenticationCapture) DurableObservationID() (string, error) {
	if !c.inputs.valid || c.runtime.config == nil || c.runtime.IsClientOwned() || c.runtime.config.authenticationBasis == nil || !c.runtime.config.authenticationBasis.valid {
		return "", errors.New("durable authentication observation is unavailable")
	}
	if err := c.runtime.RuntimeRevocation(); err != nil {
		return "", err
	}
	if c.runtime.lifetimeStore == nil {
		return "", errors.New("durable authentication observation has no owning store")
	}
	digest, err := c.runtime.lifetimeStore.authenticationDigestWithoutContext()
	if err != nil {
		return "", err
	}
	type source struct {
		Path, Raw, Evaluated string
		Exists               bool
	}
	type account struct {
		Active  string
		Entries []accounts.Entry
	}
	type binding struct {
		Owner                           providerregistry.RegistrationOwner
		Slot, Property, Source, Literal string
		References                      ProviderConfig
	}
	type provider struct {
		Owner       providerregistry.RegistrationOwner
		Config      ProviderConfig
		Template    string
		ExtraParams map[string]string
		Bindings    []binding
		Manifest    any
		Bundle      string
	}
	var files []selectedTokenInputProof
	for _, file := range c.inputs.files {
		files = append(files, selectedTokenLegacyProof(file))
	}
	var sources []source
	for _, path := range c.runtime.config.authenticationBasis.order {
		value, ok := c.runtime.config.authenticationBasis.sources[path]
		if !ok {
			return "", errAuthenticationBasisUnavailable
		}
		sources = append(sources, source{path, stableBytesID(value.raw), stableBytesID(value.evaluated), value.exists})
	}
	selected := map[string]account{}
	var providers []provider
	for _, owner := range c.owners {
		if owner.AccountNamespace != "" {
			selected[owner.AccountNamespace] = account{c.accounts.ActiveID(owner.AccountNamespace), c.accounts.Entries(owner.AccountNamespace)}
		}
		p, _ := c.runtime.config.authenticationCollectionProvider(owner.ProviderID)
		item := provider{Owner: owner, Config: p, Template: p.APIKeyTemplate, ExtraParams: p.ExtraParams}
		if b := p.resolvedAPIKey; b != nil {
			if !b.matches(p) {
				return "", errResolvedProviderAPIKeyStale
			}
			item.Bindings = append(item.Bindings, binding{b.owner, b.slot, "", b.source, b.literal, b.references})
		}
		for _, id := range slices.Sorted(maps.Keys(p.resolvedCredentials)) {
			b := p.resolvedCredentials[id]
			if b == nil || !b.matches(p) {
				return "", errResolvedConfigurationCredential
			}
			item.Bindings = append(item.Bindings, binding{b.owner, b.slot, b.property, b.source, b.literal, b.references})
		}
		if b := p.resolvedEndpoint; b != nil {
			if !b.matches(p) {
				return "", errResolvedProviderEndpointStale
			}
			item.Bindings = append(item.Bindings, binding{b.owner, "provider.base_url", "", b.source, b.literal, b.references})
		}
		if owner.HasManifest {
			r, ok := c.runtime.registry.Lookup(owner.ProviderID)
			if !ok || r.Owner() != owner || c.runtime.config.providerScan == nil {
				return "", errors.New("durable authentication owner definition changed")
			}
			status, found := c.runtime.config.providerScan.pluginStatuses[owner.ManifestID]
			if !found {
				return "", errors.New("durable authentication bundle is unavailable")
			}
			item.Manifest, item.Bundle = r.Manifest, status.Digest
		}
		providers = append(providers, item)
	}
	data, err := json.Marshal([]any{c.runtime.config, c.owners, c.inputs.order, files, sources, selected, providers, stableEnvironmentID(c.runtime.Environment()), c.runtime.config.authenticationBasis.noUnset})
	if err != nil {
		return "", errors.New("durable authentication observation cannot be encoded")
	}
	return digest.bytesID(authenticationDigestDurableObservation, data), nil
}

// RestoreAuthenticationProposal attaches provenance only after current saved
// state matches the retained observation and produces the exact original wire
// digest. It never treats JSON alone as an original capture or changes a token.
func (s *ConfigStore) RestoreAuthenticationProposal(ctx context.Context, proposal RemoteRuntimeProposal, observation string, removed map[providerregistry.RegistrationOwner]bool) (AuthenticationCapture, RemoteRuntimeProposal, error) {
	current, err := s.CaptureAuthentication(ctx)
	if err != nil {
		return AuthenticationCapture{}, RemoteRuntimeProposal{}, err
	}
	id, err := current.DurableObservationID()
	if err != nil || observation == "" || id != observation {
		return AuthenticationCapture{}, RemoteRuntimeProposal{}, errors.New("recorded authentication capture differs; reload and review saved state explicitly")
	}
	collected, err := s.CollectRemoteRuntimeForAuthentication(ctx, current, proposal.Revision, removed)
	if err != nil {
		return AuthenticationCapture{}, RemoteRuntimeProposal{}, err
	}
	digest, err := RemoteRuntimeDigest(proposal)
	if err != nil || digest != proposal.Digest || collected.Digest != proposal.Digest {
		return AuthenticationCapture{}, RemoteRuntimeProposal{}, errors.New("recorded authentication proposal differs from current captured inputs")
	}
	return current, collected, nil
}

// RegisterAuthenticationProposalSecrets protects restored private history even
// before it can be adopted. It performs no compilation, resolution or I/O.
func RegisterAuthenticationProposalSecrets(p RemoteRuntimeProposal) {
	for _, d := range p.Providers {
		registerProviderSecrets(d.Config, providerregistry.Registration{}, false)
		// A restored bundle has not been admitted yet; conservatively register
		// its configuration values until declaration validation is available.
		redact.RegisterJSONValue(d.Config.Configuration)
		if d.NativeIdentity != nil {
			redact.Register(d.NativeIdentity.UserAgent, d.NativeIdentity.Version, d.NativeIdentity.Originator)
		}
		if d.GeminiProjectID != nil {
			redact.Register(*d.GeminiProjectID)
		}
	}
	for _, b := range p.Credentials {
		redact.Register(b.APIKey)
		registerOAuthTokenSecrets(b.OAuthToken)
		if b.Account != nil {
			registerAccountSecrets(*b.Account)
		}
	}
	registerImageBrowserSecrets(p.ImageBrowserCredentials)
	for _, id := range p.ImageClientIdentities {
		redact.Register(id.Version, id.UserAgent)
	}
	redact.RegisterJSONValue(p.CredentialEnvironment)
	for _, value := range p.ProviderContextInstructions {
		redact.Register(value)
	}
}
