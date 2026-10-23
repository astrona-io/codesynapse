package memstore

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// scheduleTable is shared by a store and its transaction snapshots, like jobs.
type scheduleTable struct {
	mu     sync.Mutex
	all    map[string]accounts.Schedule
	leases map[string]lease
}

type lease struct {
	owner string
	until time.Time
}

func (s *Store) scheduleTable() *scheduleTable {
	s.schedOnce.Do(func() {
		if s.sched == nil {
			s.sched = &scheduleTable{all: map[string]accounts.Schedule{}, leases: map[string]lease{}}
		}
	})
	return s.sched
}

// ScheduleByRepo implements accounts.Store.
func (s *Store) ScheduleByRepo(_ context.Context, repo string) (accounts.Schedule, error) {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	sc, ok := t.all[repo]
	if !ok {
		return accounts.Schedule{}, fmt.Errorf("schedule: %w", accounts.ErrNotFound)
	}
	return sc, nil
}

// PutSchedule implements accounts.Store.
func (s *Store) PutSchedule(_ context.Context, sc accounts.Schedule) error {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.all[sc.Repo] = sc
	return nil
}

// EnsureSchedule implements accounts.Store.
func (s *Store) EnsureSchedule(_ context.Context, sc accounts.Schedule) error {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.all[sc.Repo]; !ok {
		t.all[sc.Repo] = sc
	}
	return nil
}

// DeleteSchedule implements accounts.Store.
func (s *Store) DeleteSchedule(_ context.Context, repo string) error {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.all, repo)
	return nil
}

// DueSchedules implements accounts.Store.
func (s *Store) DueSchedules(_ context.Context, now time.Time) ([]string, error) {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []string{}
	for repo, sc := range t.all {
		if sc.IntervalMinutes > 0 && !sc.Paused && !sc.NextRun.IsZero() && !now.Before(sc.NextRun) {
			out = append(out, repo)
		}
	}
	slices.Sort(out)
	return out[:min(len(out), accounts.MaxSchedules)], nil
}

// RecordScheduleRun implements accounts.Store.
func (s *Store) RecordScheduleRun(_ context.Context, repo string, run accounts.ScheduleRun) error {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	sc, ok := t.all[repo]
	if !ok {
		return nil // removed meanwhile
	}
	if !run.LastRun.IsZero() {
		sc.LastRun = run.LastRun
	}
	sc.LastStatus, sc.LastError = run.LastStatus, run.LastError
	if !run.NextRun.IsZero() && sc.IntervalMinutes > 0 && !sc.Paused {
		sc.NextRun = run.NextRun
	}
	if run.ResetMissing {
		sc.Missing = 0
	}
	t.all[repo] = sc
	return nil
}

// AddScheduleMissing implements accounts.Store.
func (s *Store) AddScheduleMissing(_ context.Context, repo string, gone bool) (int, error) {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	sc, ok := t.all[repo]
	if !ok {
		return 0, nil
	}
	if gone {
		sc.Missing++
	} else {
		sc.Missing = 0
	}
	t.all[repo] = sc
	return sc.Missing, nil
}

// TakeLease implements accounts.Store.
func (s *Store) TakeLease(_ context.Context, name, owner string, until, now time.Time) (bool, error) {
	t := s.scheduleTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.leases[name]
	if ok && l.owner != owner && now.Before(l.until) {
		return false, nil
	}
	t.leases[name] = lease{owner: owner, until: until}
	return true, nil
}
