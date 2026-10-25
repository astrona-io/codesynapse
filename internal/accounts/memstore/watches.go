package memstore

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

func watchID(orgID, key string) string { return orgID + "|" + key }

// OrgWatches implements accounts.Store.
func (s *Store) OrgWatches(_ context.Context, orgID string) ([]accounts.OrgWatch, error) {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []accounts.OrgWatch{}
	for _, w := range t.watches {
		if w.OrgID == orgID {
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(a, b accounts.OrgWatch) int { return strings.Compare(a.Key(), b.Key()) })
	return out, nil
}

// PutOrgWatch implements accounts.Store.
func (s *Store) PutOrgWatch(_ context.Context, w accounts.OrgWatch) error {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	id := watchID(w.OrgID, w.Key())
	if old, ok := t.watches[id]; ok {
		w.LastScan, w.LastStatus, w.LastError, w.NextScan, w.Added, w.Known = old.LastScan, old.LastStatus, old.LastError, old.NextScan, old.Added, old.Known
	}
	t.watches[id] = w
	return nil
}

// DeleteOrgWatch implements accounts.Store.
func (s *Store) DeleteOrgWatch(_ context.Context, orgID, key string) error {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.watches, watchID(orgID, key))
	return nil
}

// DueOrgWatches implements accounts.Store.
func (s *Store) DueOrgWatches(_ context.Context, now time.Time) ([]accounts.OrgWatch, error) {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []accounts.OrgWatch{}
	for _, w := range t.watches {
		if !w.Paused && !now.Before(w.NextScan) {
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(a, b accounts.OrgWatch) int {
		return strings.Compare(watchID(a.OrgID, a.Key()), watchID(b.OrgID, b.Key()))
	})
	return out[:min(len(out), accounts.MaxWatches)], nil
}

// RecordWatchScan implements accounts.Store. A removed watch is no error.
func (s *Store) RecordWatchScan(_ context.Context, orgID, key string, scan accounts.WatchScan) error {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	id := watchID(orgID, key)
	w, ok := t.watches[id]
	if !ok {
		return nil
	}
	if !scan.LastScan.IsZero() {
		w.LastScan = scan.LastScan
	}
	w.LastStatus, w.LastError = scan.LastStatus, scan.LastError
	if !scan.NextScan.IsZero() {
		w.NextScan = scan.NextScan
	}
	w.Added += scan.Added
	if scan.Known != nil {
		w.Known = scan.Known
	}
	t.watches[id] = w
	return nil
}
