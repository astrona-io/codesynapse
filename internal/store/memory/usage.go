package memory

import (
	"context"
	"slices"

	"github.com/astrona-io/codesynapse/internal/usage"
)

// AddUsage implements store.UsageStore.
func (s *Store) AddUsage(_ context.Context, buckets []usage.Bucket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range buckets {
		cur, ok := s.usage[b.Key()]
		if !ok {
			s.usage[b.Key()] = b
			continue
		}
		cur.Add(b)
		s.usage[b.Key()] = cur
	}
	return nil
}

// PruneUsage implements store.UsageStore. Hourly buckets of those days go
// too; they are kept for less time than day buckets anyway.
func (s *Store) PruneUsage(_ context.Context, beforeDay string) error {
	s.pruneUsage(beforeDay, false)
	return nil
}

// PruneUsageHours implements store.UsageStore.
func (s *Store) PruneUsageHours(_ context.Context, beforeDay string) error {
	s.pruneUsage(beforeDay, true)
	return nil
}

func (s *Store) pruneUsage(beforeDay string, hourlyOnly bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, b := range s.usage {
		if b.Day < beforeDay && (b.Hour != "" || !hourlyOnly) {
			delete(s.usage, k)
		}
	}
}

// Usage implements store.UsageStore.
func (s *Store) Usage(_ context.Context, sinceDay string) ([]usage.Bucket, error) {
	return s.usageSince(sinceDay, false), nil
}

// UsageHours implements store.UsageStore.
func (s *Store) UsageHours(_ context.Context, sinceDay string) ([]usage.Bucket, error) {
	return s.usageSince(sinceDay, true), nil
}

func (s *Store) usageSince(sinceDay string, hourly bool) []usage.Bucket {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []usage.Bucket{}
	for _, b := range s.usage {
		if b.Day >= sinceDay && (b.Hour != "") == hourly {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, usage.Compare)
	return out
}
