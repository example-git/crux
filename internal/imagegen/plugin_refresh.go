package imagegen

import (
	"context"
	"errors"
	"maps"
	"sync"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
)

type imageRefreshOwner struct {
	snapshot config.RuntimeSnapshot
	spent    bool
	epoch    uint64
}
type imageCredentialRefresh struct {
	mu       sync.Mutex
	store    *config.ConfigStore
	values   map[string]any
	bindings map[string]providerregistry.RegistrationOwner
	owners   map[providerregistry.RegistrationOwner]*imageRefreshOwner
	epoch    uint64
}

func newImageCredentialRefresh(store *config.ConfigStore) *imageCredentialRefresh {
	return &imageCredentialRefresh{store: store, bindings: map[string]providerregistry.RegistrationOwner{}, owners: map[providerregistry.RegistrationOwner]*imageRefreshOwner{}}
}
func (s *imageCredentialRefresh) bind(id string, owner providerregistry.RegistrationOwner, snapshot config.RuntimeSnapshot, spent bool) {
	s.bindings[id] = owner
	if existing := s.owners[owner]; existing == nil || spent {
		s.owners[owner] = &imageRefreshOwner{snapshot: snapshot, spent: spent}
	}
}
func (s *imageCredentialRefresh) read() (map[string]any, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.values), s.epoch
}
func (s *imageCredentialRefresh) refresh(ctx context.Context, ids []string, attempt uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	changed := false
	seen := map[providerregistry.RegistrationOwner]bool{}
	for _, id := range ids {
		owner, ok := s.bindings[id]
		if !ok || seen[owner] {
			continue
		}
		seen[owner] = true
		state := s.owners[owner]
		if state.epoch > attempt {
			changed = true
			continue
		}
		if state.spent {
			continue
		}
		// Consume before dispatch: a timeout or lost response never permits
		// this job to exchange the same original refresh token again.
		state.spent = true
		refreshed, err := s.store.RequestClientRefresh(ctx, state.snapshot, owner)
		if err != nil {
			return false, err
		}
		values, err := imageProviderCredential(ctx, refreshed, owner)
		if err != nil {
			return false, err
		}
		old, ok := s.values[id].(map[string]any)
		if !ok || old["base_url"] != values["base_url"] {
			return false, errors.New("image credential endpoint changed during refresh")
		}
		redact.RegisterJSONValue(values)
		s.epoch++
		state.epoch = s.epoch
		state.snapshot = refreshed
		for binding, target := range s.bindings {
			if target == owner {
				s.values[binding] = values
			}
		}
		changed = true
	}
	return changed, nil
}
