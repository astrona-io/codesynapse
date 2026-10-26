package api

import (
	"context"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// statsParallel bounds the graph reads one statistics request runs at once.
// Run one after another, the per-repository reads took about ten seconds on a
// workspace of a dozen repositories.
const statsParallel = 4

// forEach runs fn for i in [0, n), at most statsParallel at a time, and
// returns the first error. Callers write results into slot i, so the output
// keeps its order.
func forEach(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, statsParallel)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for i := range n {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, i); err != nil {
				mu.Lock()
				if first == nil {
					first = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if first == nil && ctx.Err() != nil {
		// The caller's context ended before every repository ran.
		return context.Cause(ctx)
	}
	return first
}

// ruleHits are one repository's findings counted by rule. Findings change
// only when the repository is indexed again, so they are cached by its head
// and index time instead of loading every finding node on each request.
type ruleHits map[string]ruleHit

type ruleHit struct {
	severity string
	findings int
}

const (
	ruleCacheTTL = 10 * time.Minute
	ruleCacheMax = 256
)

type ruleCacheEntry struct {
	key  string
	at   time.Time
	hits ruleHits
}

type ruleCache struct {
	mu      sync.Mutex
	entries map[string]ruleCacheEntry
}

func (s *Server) findingRules(ctx context.Context, rs RepoSummary) (ruleHits, error) {
	key := rs.Head + "|" + rs.IngestedAt
	c := &s.rules
	c.mu.Lock()
	if e, ok := c.entries[rs.Name]; ok && e.key == key && time.Since(e.at) < ruleCacheTTL {
		c.mu.Unlock()
		return e.hits, nil
	}
	c.mu.Unlock()
	findings, err := s.st.List(ctx, rs.Name, store.Filter{Kinds: []model.NodeKind{model.KindFinding}}, store.ByName, store.MaxLimit)
	if err != nil {
		return nil, err
	}
	hits := ruleHits{}
	for _, f := range findings {
		h := hits[f.Node.Rule]
		if h.findings == 0 {
			h.severity = f.Node.Severity
		}
		h.findings++
		hits[f.Node.Rule] = h
	}
	c.mu.Lock()
	if c.entries == nil || len(c.entries) >= ruleCacheMax {
		c.entries = map[string]ruleCacheEntry{}
	}
	c.entries[rs.Name] = ruleCacheEntry{key: key, at: time.Now(), hits: hits}
	c.mu.Unlock()
	return hits, nil
}
