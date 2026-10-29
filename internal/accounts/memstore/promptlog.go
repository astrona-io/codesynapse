package memstore

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// promptTable holds the gateway prompt log. Like jobs it is shared by a
// store and its transaction snapshots.
type promptTable struct {
	mu       sync.Mutex
	settings map[string]accounts.PromptLogSetting
	rows     []accounts.PromptLogEntry // ascending ID
	lastID   int64
	segments []accounts.PromptLogSegment
	segID    int64
}

func (s *Store) promptTable() *promptTable {
	s.promptsOnce.Do(func() {
		if s.prompts == nil {
			s.prompts = &promptTable{settings: map[string]accounts.PromptLogSetting{}}
		}
	})
	return s.prompts
}

func (s *Store) hasOrg(orgID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.orgs[orgID]
	return ok
}

// PromptLogSetting implements accounts.Store.
func (s *Store) PromptLogSetting(_ context.Context, orgID string) (accounts.PromptLogSetting, error) {
	t := s.promptTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	if set, ok := t.settings[orgID]; ok {
		return set, nil
	}
	return accounts.DefaultPromptLogSetting(), nil
}

// SetPromptLogSetting implements accounts.Store.
func (s *Store) SetPromptLogSetting(_ context.Context, orgID string, set accounts.PromptLogSetting) error {
	if !s.hasOrg(orgID) {
		return fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	t := s.promptTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.settings[orgID] = set
	return nil
}

// AddPromptLog implements accounts.Store.
func (s *Store) AddPromptLog(_ context.Context, e accounts.PromptLogEntry) (accounts.PromptLogEntry, error) {
	if !s.hasOrg(e.OrgID) {
		return e, fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	t := s.promptTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastID++
	e.ID = t.lastID
	t.rows = append(t.rows, e)
	return e, nil
}

// ListPromptLog implements accounts.Store.
func (s *Store) ListPromptLog(_ context.Context, f accounts.PromptLogFilter) ([]accounts.PromptLogEntry, error) {
	if f.OrgID == "" {
		return nil, fmt.Errorf("org: %w", accounts.ErrInvalidInput)
	}
	limit := f.Limit
	if limit <= 0 || limit > accounts.MaxPromptLogPage {
		limit = accounts.MaxPromptLogPage
	}
	t := s.promptTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []accounts.PromptLogEntry{}
	for i := range t.rows {
		e := t.rows[i]
		if !f.Ascending {
			e = t.rows[len(t.rows)-1-i]
		}
		if accounts.MatchPromptLog(e, f) {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// PromptLogOrgs implements accounts.Store.
func (s *Store) PromptLogOrgs(_ context.Context) (map[string]int, error) {
	t := s.promptTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]int{}
	for _, e := range t.rows {
		set, ok := t.settings[e.OrgID]
		if !ok {
			set = accounts.DefaultPromptLogSetting()
		}
		out[e.OrgID] = set.RetentionDays
	}
	return out, nil
}

// PrunePromptLog implements accounts.Store.
func (s *Store) PrunePromptLog(_ context.Context, orgID string, before time.Time) (int, error) {
	t := s.promptTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.rows)
	t.rows = slices.DeleteFunc(t.rows, func(e accounts.PromptLogEntry) bool {
		return e.OrgID == orgID && e.At.Before(before)
	})
	return n - len(t.rows), nil
}

// SealPromptLogSegment implements accounts.Store.
func (s *Store) SealPromptLogSegment(_ context.Context, seg accounts.PromptLogSegment) (accounts.PromptLogSegment, error) {
	t := s.promptTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	in := func(e accounts.PromptLogEntry) bool {
		return e.OrgID == seg.OrgID && e.ID >= seg.FirstID && e.ID <= seg.LastID
	}
	n := 0
	for _, e := range t.rows {
		if in(e) {
			n++
		}
	}
	if n == 0 || n != seg.Count {
		return seg, accounts.ErrAuditConflict
	}
	t.rows = slices.DeleteFunc(t.rows, in)
	t.segID++
	seg.ID = t.segID
	seg.FirstAt, seg.LastAt, seg.SealedAt = seg.FirstAt.UTC(), seg.LastAt.UTC(), seg.SealedAt.UTC()
	t.segments = append(t.segments, seg)
	return seg, nil
}

// PromptLogSegments implements accounts.Store.
func (s *Store) PromptLogSegments(_ context.Context, before time.Time) ([]accounts.PromptLogSegment, error) {
	t := s.promptTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []accounts.PromptLogSegment{}
	for _, g := range t.segments {
		if g.LastAt.Before(before) {
			out = append(out, g)
		}
	}
	slices.SortStableFunc(out, func(a, b accounts.PromptLogSegment) int { return a.LastAt.Compare(b.LastAt) })
	return out[:min(len(out), accounts.MaxPromptLogSegments)], nil
}

// DropPromptLogSegment implements accounts.Store.
func (s *Store) DropPromptLogSegment(_ context.Context, id int64) error {
	t := s.promptTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	i := slices.IndexFunc(t.segments, func(g accounts.PromptLogSegment) bool { return g.ID == id })
	if i < 0 {
		return fmt.Errorf("prompt log segment: %w", accounts.ErrNotFound)
	}
	t.segments = slices.Delete(t.segments, i, i+1)
	return nil
}
