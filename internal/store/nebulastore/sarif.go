package nebulastore

import (
	"context"

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
	return s.setChunked(ctx, sarifKey(ns), store.RuntimeGraph(ns), set)
}

// SARIFFindings implements store.SARIFStore.
func (s *Store) SARIFFindings(ctx context.Context, ns string) (sarifimport.Set, error) {
	var set sarifimport.Set
	if err := s.getChunked(ctx, sarifKey(ns), &set); err != nil {
		return sarifimport.Set{}, err
	}
	return set, nil
}

// DeleteSARIFFindings implements store.SARIFStore.
func (s *Store) DeleteSARIFFindings(ctx context.Context, ns string) error {
	return s.deleteChunked(ctx, sarifKey(ns))
}
