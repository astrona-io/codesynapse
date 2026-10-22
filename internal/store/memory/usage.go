package memory

import (
	"cmp"
	"context"
	"slices"

	"github.com/parisnakitakejser/codesynapse/internal/usage"
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

// PruneUsage implements store.UsageStore.
func (s *Store) PruneUsage(_ context.Context, beforeDay string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, b := range s.usage {
		if b.Day < beforeDay {
			delete(s.usage, k)
		}
	}
	return nil
}

// Usage implements store.UsageStore.
func (s *Store) Usage(_ context.Context, sinceDay string) ([]usage.Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []usage.Bucket{}
	for _, b := range s.usage {
		if b.Day >= sinceDay {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b usage.Bucket) int {
		return cmp.Or(cmp.Compare(a.Day, b.Day), cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Tool, b.Tool), cmp.Compare(a.Client, b.Client))
	})
	return out, nil
}
