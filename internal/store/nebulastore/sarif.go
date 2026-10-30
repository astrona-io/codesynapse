package nebulastore

import (
	"context"
	"errors"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/sarifimport"
	"github.com/astrona-io/codesynapse/internal/store"
)

var _ store.SARIFStore = (*Store)(nil)

// Imported SARIF findings are a chunked document like runtime facts, under
// their own key: a namespace ("graph" or "org|graph") never contains ":",
// so "sarif:"+ns cannot meet a runtime facts key, and graph_name is the
// namespace's graph, so deleting the graph drops them too.
func sarifKey(ns string) string { return "sarif:" + ns }

// SetSARIFFindings implements store.SARIFStore.
func (s *Store) SetSARIFFindings(ctx context.Context, ns string, set sarifimport.Set) error {
	if err := s.setChunked(ctx, sarifKey(ns), store.RuntimeGraph(ns), set); err != nil {
		return fmt.Errorf("sarif findings: %w", err)
	}
	return nil
}

// SARIFFindings implements store.SARIFStore.
func (s *Store) SARIFFindings(ctx context.Context, ns string) (sarifimport.Set, error) {
	var set sarifimport.Set
	if err := s.getChunked(ctx, sarifKey(ns), &set); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return sarifimport.Set{}, err
		}
		return sarifimport.Set{}, fmt.Errorf("sarif findings: %w", err)
	}
	return set, nil
}

// DeleteSARIFFindings implements store.SARIFStore.
func (s *Store) DeleteSARIFFindings(ctx context.Context, ns string) error {
	if err := s.deleteChunked(ctx, sarifKey(ns)); err != nil {
		return fmt.Errorf("sarif findings: %w", err)
	}
	return nil
}
