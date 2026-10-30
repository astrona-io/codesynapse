package memory

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/sarifimport"
	"github.com/astrona-io/codesynapse/internal/store"
)

var _ store.SARIFStore = (*Store)(nil)

// SetSARIFFindings implements store.SARIFStore.
func (s *Store) SetSARIFFindings(_ context.Context, ns string, set sarifimport.Set) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sarif == nil {
		s.sarif = map[string]sarifimport.Set{}
	}
	s.sarif[ns] = set
	return nil
}

// SARIFFindings implements store.SARIFStore.
func (s *Store) SARIFFindings(_ context.Context, ns string) (sarifimport.Set, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set, ok := s.sarif[ns]
	if !ok {
		return sarifimport.Set{}, store.ErrNotFound
	}
	return set, nil
}

// DeleteSARIFFindings implements store.SARIFStore.
func (s *Store) DeleteSARIFFindings(_ context.Context, ns string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sarif, ns)
	return nil
}
