package memstore

import (
	"context"
	"slices"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// ImageScan implements accounts.Store.
func (s *Store) ImageScan(_ context.Context, ns string) (accounts.ImageScan, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sc, ok := s.imageScans[ns]
	sc.Findings = slices.Clone(sc.Findings)
	return sc, ok, nil
}

// SetImageScan implements accounts.Store.
func (s *Store) SetImageScan(_ context.Context, ns string, sc accounts.ImageScan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.imageScans == nil {
		s.imageScans = map[string]accounts.ImageScan{}
	}
	sc.Findings = slices.Clone(sc.Findings)
	s.imageScans[ns] = sc
	return nil
}

// DeleteImageScan implements accounts.Store.
func (s *Store) DeleteImageScan(_ context.Context, ns string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.imageScans, ns)
	return nil
}
