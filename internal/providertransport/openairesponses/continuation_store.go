package openairesponses

import "sync"

type continuationKey struct {
	owner        string
	model        string
	conversation string
	purpose      string
}

type ContinuationStore struct {
	mu     sync.Mutex
	chains map[continuationKey]*continuationChain
	closed bool
}

func NewContinuationStore() *ContinuationStore {
	return &ContinuationStore{chains: make(map[continuationKey]*continuationChain)}
}

func (s *ContinuationStore) chain(key continuationKey) *continuationChain {
	if s == nil {
		return &continuationChain{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &continuationChain{}
	}
	chain := s.chains[key]
	if chain == nil {
		chain = &continuationChain{}
		s.chains[key] = chain
	}
	return chain
}

func (s *ContinuationStore) resetModelConversation(owner, model, conversation string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.chains {
		if key.owner == owner && key.model == model && key.conversation == conversation {
			delete(s.chains, key)
		}
	}
}

func (s *ContinuationStore) ResetConversation(conversation string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.chains {
		if key.conversation == conversation {
			delete(s.chains, key)
		}
	}
}

func (s *ContinuationStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.chains = nil
	s.mu.Unlock()
}
