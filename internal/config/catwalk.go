package config

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
)

var _ syncer[[]catwalk.Provider] = (*catwalkSync)(nil)

type catwalkSync struct {
	once   sync.Once
	result []catwalk.Provider
	cache  cache[[]catwalk.Provider]
	init   atomic.Bool
}

func retainedCatalogProviders(providers []catwalk.Provider) []catwalk.Provider {
	return slices.DeleteFunc(slices.Clone(providers), func(provider catwalk.Provider) bool {
		return provider.Type != catwalk.TypeOpenAICompat && provider.ID != catwalk.InferenceProviderCopilot
	})
}

func (s *catwalkSync) Init(path string) {
	s.cache = newCache[[]catwalk.Provider](path)
	s.init.Store(true)
}

func (s *catwalkSync) Get(context.Context) ([]catwalk.Provider, error) {
	if !s.init.Load() {
		panic("called Get before Init")
	}

	s.once.Do(func() {
		cached, _, err := s.cache.Get()
		if err == nil && len(cached) > 0 {
			s.result = retainedCatalogProviders(cached)
			return
		}
		s.result = retainedCatalogProviders(embedded.GetAll())
	})
	return s.result, nil
}
