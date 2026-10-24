package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
)

const (
	overviewCacheTTL = 30 * time.Second
	overviewCacheMax = 256
)

type overviewMetrics struct {
	stats    model.Stats
	security security.Report
	called   int
}

type overviewCacheEntry struct {
	key   [32]byte
	at    time.Time
	ready chan struct{}
	data  overviewMetrics
	err   error
}

// Only raw code-graph aggregates are shared. Authorisation, tenant data,
// schedules, job health and index age are evaluated on every request.
type overviewCache struct {
	mu      sync.Mutex
	entries map[string]*overviewCacheEntry
}

func (s *Server) overviewMetrics(ctx context.Context, repo model.Node) (overviewMetrics, error) {
	// Include the publication receipt and all repository metadata: reindexing
	// the same commit must also invalidate the cached figures.
	b, err := json.Marshal(nodeprops.To(repo))
	if err != nil {
		return overviewMetrics{}, err
	}
	key := sha256.Sum256(b)
	c := &s.overviews
	c.mu.Lock()
	if e, ok := c.entries[repo.Name]; ok && e.key == key && time.Since(e.at) < overviewCacheTTL {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return overviewMetrics{}, ctx.Err()
		case <-e.ready:
			return e.data, e.err
		}
	}
	if c.entries == nil || len(c.entries) >= overviewCacheMax {
		c.entries = map[string]*overviewCacheEntry{}
	}
	e := &overviewCacheEntry{key: key, at: time.Now(), ready: make(chan struct{})}
	c.entries[repo.Name] = e
	c.mu.Unlock()

	e.data.stats, e.err = s.st.Stats(ctx, repo.Name)
	if e.err == nil {
		var sec security.Report
		sec, e.err = security.Build(ctx, s.st, repo.Name, security.Options{SummaryOnly: true})
		if e.err == nil {
			for _, v := range sec.Vulnerabilities {
				if v.Reachability == "called" {
					e.data.called++
				}
			}
			// The overview needs aggregates only; do not retain finding bodies
			// or dependency details for every cached repository.
			e.data.security = security.Report{Grade: sec.Grade, Score: sec.Score,
				Modules: sec.Modules, FindingCounts: sec.FindingCounts, VulnCounts: sec.VulnCounts}
		}
	}
	c.mu.Lock()
	if e.err != nil && c.entries[repo.Name] == e {
		delete(c.entries, repo.Name)
	}
	close(e.ready)
	c.mu.Unlock()
	return e.data, e.err
}
