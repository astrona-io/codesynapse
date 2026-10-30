package memory

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/store"
)

// SetRuntimeFacts implements store.RuntimeStore.
func (s *Store) SetRuntimeFacts(_ context.Context, ns string, f runtimefacts.Facts) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime == nil {
		s.runtime = map[string]runtimefacts.Facts{}
	}
	s.runtime[ns] = f
	return nil
}

// RuntimeFacts implements store.RuntimeReader.
func (s *Store) RuntimeFacts(_ context.Context, ns string) (runtimefacts.Facts, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.runtime[ns]
	if !ok {
		return runtimefacts.Facts{}, store.ErrNotFound
	}
	return f, nil
}

// DeleteRuntimeFacts implements store.RuntimeStore.
func (s *Store) DeleteRuntimeFacts(_ context.Context, ns string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runtime, ns)
	return nil
}

// deleteRuntimeOf drops every namespace's facts and imported findings of
// graph; the caller holds mu.
func (s *Store) deleteRuntimeOf(graph string) {
	for ns := range s.runtime {
		if store.RuntimeGraph(ns) == graph {
			delete(s.runtime, ns)
		}
	}
	for ns := range s.sarif {
		if store.RuntimeGraph(ns) == graph {
			delete(s.sarif, ns)
		}
	}
}
