package config

import (
	"errors"
	"fmt"
)

// runtimeCollectionSource binds the original, possibly unresolved local config
// to one successfully collected proposal. It stays on the owning client; JSON
// transport cannot create this proof or carry its captured host environment.
type runtimeCollectionSource struct {
	runtime RuntimeSnapshot
	digest  string
}

func (runtimeCollectionSource) MarshalJSON() ([]byte, error) {
	return nil, errors.New("runtime collection sources are private")
}

func (runtimeCollectionSource) String() string   { return "[private runtime collection source]" }
func (runtimeCollectionSource) GoString() string { return "[private runtime collection source]" }
func (runtimeCollectionSource) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private runtime collection source]"))
}

// CollectionConfig returns the read-only config admitted by successful local
// collection. Manually constructed, decoded and rebound proposals return nil.
// Callers must not mutate the returned config, just as with RuntimeSnapshot.
func (p RemoteRuntimeProposal) CollectionConfig() *Config {
	if p.collectionSource == nil || p.collectionSource.digest == "" || p.collectionSource.digest != p.Digest {
		return nil
	}
	return p.collectionSource.runtime.Config()
}

// RetainCollectionSource preserves private local provenance across a JSON copy
// only when the copied wire content still has the source's collected digest.
// A source without provenance clears it, preserving manual-proposal support.
// This operation performs no credential resolution or filesystem access.
func (p *RemoteRuntimeProposal) RetainCollectionSource(source RemoteRuntimeProposal) error {
	if p == nil {
		return errors.New("runtime collection destination is unavailable")
	}
	if source.collectionSource == nil {
		p.collectionSource = nil
		return nil
	}
	if source.CollectionConfig() == nil || source.Digest != p.Digest {
		return errors.New("runtime collection source digest changed")
	}
	digest, err := RemoteRuntimeDigest(*p)
	if err != nil || digest != p.Digest {
		return errors.New("runtime collection content does not match its source")
	}
	p.collectionSource = source.collectionSource
	return nil
}
