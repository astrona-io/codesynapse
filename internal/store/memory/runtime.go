package memory

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/store"
)

// SetRuntimeFacts replaces the measured facts of repo.
func (s *Store) SetRuntimeFacts(_ context.Context, repo string, f runtimefacts.Facts) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime == nil {
		s.runtime = map[string]runtimefacts.Facts{}
	}
	s.runtime[repo] = f
	return nil
}

// RuntimeFacts implements store.RuntimeReader.
func (s *Store) RuntimeFacts(_ context.Context, repo string) (runtimefacts.Facts, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.runtime[repo]
	if !ok {
		return runtimefacts.Facts{}, store.ErrNotFound
	}
	return f, nil
}
