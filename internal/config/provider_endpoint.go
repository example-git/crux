package config

import (
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/providerregistry"
)

var errResolvedProviderEndpointStale = errors.New("resolved provider endpoint no longer matches its owner or source")

// Independent from the credential: replacing a key cannot turn a resolved URL
// back into executable shell input. JSON/reload deliberately drops this marker.
type resolvedProviderEndpoint struct {
	owner           providerregistry.RegistrationOwner
	source, literal string
	references      ProviderConfig
}

func (resolvedProviderEndpoint) MarshalJSON() ([]byte, error) {
	return nil, errors.New("resolved provider endpoints are private")
}

func (resolvedProviderEndpoint) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private resolved provider endpoint]"))
}

func (b *resolvedProviderEndpoint) matches(p ProviderConfig) bool {
	return b != nil && p.ID == b.owner.ProviderID && p.BaseURL == b.literal && providerOwnershipReferencesMatch(p, b.references)
}

func bindResolvedProviderEndpoint(snapshot RuntimeSnapshot, p ProviderConfig, owner providerregistry.RegistrationOwner, source, literal string) (ProviderConfig, error) {
	actual, ok := snapshot.ProviderOwnerFor(p.ID, p)
	current, active := snapshot.ProviderOwner(p.ID)
	if !ok || !active || actual != owner || current != owner {
		return ProviderConfig{}, errResolvedProviderEndpointStale
	}
	p = cloneProviderConfig(p)
	p.BaseURL = literal
	p.resolvedEndpoint = &resolvedProviderEndpoint{owner: owner, source: source, literal: literal, references: ProviderConfig{ID: p.ID, Owner: clonePointer(p.Owner), Plugin: clonePointer(p.Plugin), Preset: clonePointer(p.Preset)}}
	return p, nil
}

// ResolveProviderEndpoint consumes an already resolved endpoint literally.
// Snapshot callers additionally check its complete current owner/slot below.
func ResolveProviderEndpoint(p ProviderConfig, resolve func(string) (string, error)) (string, error) {
	if p.resolvedEndpoint != nil {
		if !p.resolvedEndpoint.matches(p) {
			return "", errResolvedProviderEndpointStale
		}
		return p.resolvedEndpoint.literal, nil
	}
	return resolve(p.BaseURL)
}

func (s RuntimeSnapshot) validateResolvedProviderEndpointOwner(p ProviderConfig) error {
	if p.resolvedEndpoint == nil {
		return nil
	}
	owner, ok := s.ProviderOwnerFor(p.ID, p)
	current, active := s.ProviderOwner(p.ID)
	if !ok || !active || owner != current || owner != p.resolvedEndpoint.owner || !p.resolvedEndpoint.matches(p) || s.config.Providers == nil {
		return errResolvedProviderEndpointStale
	}
	stored, found := s.config.Providers.Get(p.ID)
	if !found || !p.resolvedEndpoint.matches(stored) || !stored.resolvedEndpoint.matches(stored) || stored.resolvedEndpoint.owner != owner || stored.resolvedEndpoint.source != p.resolvedEndpoint.source {
		return errResolvedProviderEndpointStale
	}
	return nil
}

func (s RuntimeSnapshot) ResolveProviderEndpoint(p ProviderConfig) (string, error) {
	if err := s.RuntimeRevocation(); err != nil {
		return "", err
	}
	if err := s.validateResolvedProviderEndpointOwner(p); err != nil {
		return "", err
	}
	return ResolveProviderEndpoint(p, s.Resolve)
}
