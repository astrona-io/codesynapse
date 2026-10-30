package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// jobTable is shared by a store and its transaction snapshots: jobs are not
// part of policy transactions.
type jobTable struct {
	mu   sync.Mutex
	jobs map[string]accounts.JobRecord
	// wakes are the channels JobWake handed out.
	wakes []chan struct{}
}

// JobWake implements accounts.JobWaker.
func (s *Store) JobWake(ctx context.Context) <-chan struct{} {
	t := s.jobTable()
	ch := make(chan struct{}, 1)
	t.mu.Lock()
	t.wakes = append(t.wakes, ch)
	t.mu.Unlock()
	go func() {
		<-ctx.Done()
		t.mu.Lock()
		t.wakes = slices.DeleteFunc(t.wakes, func(c chan struct{}) bool { return c == ch })
		t.mu.Unlock()
	}()
	return ch
}

// wakeLocked tells every waiting worker a job was added; t.mu is held.
func (t *jobTable) wakeLocked() {
	for _, ch := range t.wakes {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Store) jobTable() *jobTable {
	s.jobsOnce.Do(func() {
		if s.jobs == nil {
			s.jobs = &jobTable{jobs: map[string]accounts.JobRecord{}}
		}
	})
	return s.jobs
}

// AddJob implements accounts.Store.
func (s *Store) AddJob(_ context.Context, j accounts.JobRecord) error {
	if err := accounts.ValidateJob(j); err != nil {
		return err
	}
	t := s.jobTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.jobs[j.ID]; ok {
		return fmt.Errorf("job: %w", accounts.ErrExists)
	}
	for _, x := range t.jobs {
		if x.Repo == j.Repo && x.Active() {
			return accounts.ErrJobActive
		}
	}
	j.Params = slices.Clone(j.Params)
	t.jobs[j.ID] = j
	t.wakeLocked()
	return nil
}

// JobByID implements accounts.Store.
func (s *Store) JobByID(_ context.Context, id string) (accounts.JobRecord, error) {
	t := s.jobTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok {
		return accounts.JobRecord{}, fmt.Errorf("job: %w", accounts.ErrNotFound)
	}
	j.Params = slices.Clone(j.Params)
	return j, nil
}

// ListJobs implements accounts.Store.
func (s *Store) ListJobs(_ context.Context, orgIDs []string, limit int) ([]accounts.JobRecord, error) {
	t := s.jobTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []accounts.JobRecord{}
	for _, j := range t.jobs {
		if orgIDs == nil || slices.Contains(orgIDs, j.OrgID) {
			j.Params = nil
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b accounts.JobRecord) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out[:min(len(out), max(limit, 1))], nil
}

// StartJob implements accounts.Store.
func (s *Store) StartJob(_ context.Context, id, owner string, leaseUntil, now time.Time) error {
	t := s.jobTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok || j.LeaseOwner != owner || j.State != accounts.JobQueued {
		return fmt.Errorf("job: %w", accounts.ErrNotFound)
	}
	j.State, j.Attempts, j.LeaseUntil, j.StartedAt = accounts.JobRunning, j.Attempts+1, leaseUntil, &now
	t.jobs[id] = j
	return nil
}

// RenewJobLeases implements accounts.Store.
func (s *Store) RenewJobLeases(_ context.Context, owner string, until time.Time) error {
	t := s.jobTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, j := range t.jobs {
		if j.LeaseOwner == owner && j.Active() {
			j.LeaseUntil = until
			t.jobs[id] = j
		}
	}
	return nil
}

// FinishJob implements accounts.Store.
func (s *Store) FinishJob(_ context.Context, id, owner string, out accounts.JobOutcome) error {
	t := s.jobTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok || j.LeaseOwner != owner || !j.Active() {
		return fmt.Errorf("job: %w", accounts.ErrNotFound)
	}
	t.jobs[id] = finished(j, out)
	t.prune()
	return nil
}

func finished(j accounts.JobRecord, out accounts.JobOutcome) accounts.JobRecord {
	at := out.At
	j.State, j.Result, j.Error, j.Nodes, j.Edges, j.FinishedAt, j.Params, j.LeaseOwner = out.State, out.Result, out.Error, out.Nodes, out.Edges, &at, nil, ""
	return j
}

// ClaimOrphanedJob implements accounts.Store.
func (s *Store) ClaimOrphanedJob(_ context.Context, owner string, kinds []string, leaseUntil, now time.Time) (accounts.JobRecord, bool, error) {
	t := s.jobTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		var oldest *accounts.JobRecord
		for _, j := range t.jobs {
			if j.Active() && j.LeaseUntil.Before(now) && (kinds == nil || slices.Contains(kinds, j.Kind)) && (oldest == nil || j.CreatedAt.Before(oldest.CreatedAt)) {
				oldest = &j
			}
		}
		if oldest == nil {
			return accounts.JobRecord{}, false, nil
		}
		j := *oldest
		if j.Attempts >= accounts.MaxJobAttempts {
			t.jobs[j.ID] = finished(j, accounts.JobOutcome{State: accounts.JobFailed, At: now,
				Error: fmt.Sprintf("interrupted %d times; not started again", j.Attempts)})
			continue
		}
		j.State, j.LeaseOwner, j.LeaseUntil = accounts.JobQueued, owner, leaseUntil
		t.jobs[j.ID] = j
		j.Params = slices.Clone(j.Params)
		return j, true, nil
	}
}

// JobQueue implements accounts.Store.
func (s *Store) JobQueue(_ context.Context, now time.Time) (accounts.JobQueue, error) {
	t := s.jobTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	q := accounts.JobQueue{Waiting: map[string]int{}, Active: map[string]int{}, Oldest: map[string]time.Time{}}
	for _, j := range t.jobs {
		if !j.Active() {
			continue
		}
		q.Active[j.Kind]++
		expired := j.LeaseUntil.Before(now)
		if expired {
			q.Waiting[j.Kind]++
		}
		if old, ok := q.Oldest[j.Kind]; (expired || j.State == accounts.JobQueued) && (!ok || j.CreatedAt.Before(old)) {
			q.Oldest[j.Kind] = j.CreatedAt
		}
	}
	return q, nil
}

// prune keeps the newest accounts.JobHistory finished jobs. Callers hold mu.
func (t *jobTable) prune() {
	var done []accounts.JobRecord
	for _, j := range t.jobs {
		if !j.Active() {
			done = append(done, j)
		}
	}
	if len(done) <= accounts.JobHistory {
		return
	}
	slices.SortFunc(done, func(a, b accounts.JobRecord) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, j := range done[:len(done)-accounts.JobHistory] {
		delete(t.jobs, j.ID)
	}
}
