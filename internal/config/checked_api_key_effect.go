package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/example-git/crux/internal/providerregistry"
)

// ConfiguredCredentialEffectID retains the exact checked source and literal,
// even when a later save stops partially. It is private intent proof only.
func (p CheckedAPIKeyPreparation) ConfiguredCredentialEffectID() (string, error) {
	if !p.valid || p.owner.ProviderID == "" || p.slot.ID == "" {
		return "", errors.New("checked credential preparation is unavailable")
	}
	source, literal := "", ""
	if p.slot.Property == "" {
		b := p.provider.resolvedAPIKey
		if b == nil || b.owner != p.owner || !b.matches(p.provider) {
			return "", errors.New("checked credential source changed")
		}
		source, literal = b.source, b.literal
	} else {
		for _, b := range p.provider.resolvedCredentials {
			if b != nil && b.owner == p.owner && b.property == p.slot.Property && b.matches(p.provider) {
				source, literal = b.source, b.literal
				break
			}
		}
	}
	if source == "" || literal == "" {
		return "", errors.New("checked credential source and literal are unavailable")
	}
	data, err := json.Marshal(struct {
		Owner                           providerregistry.RegistrationOwner
		Slot, Property, Source, Literal string
	}{p.owner, p.slot.ID, p.slot.Property, source, literal})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
