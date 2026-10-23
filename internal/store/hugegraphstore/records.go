package hugegraphstore

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// Reviews and usage counters are their own vertex labels, so repository
// replacement and deletion (label "node") never touch them. A review is one
// JSON document.

const (
	saveReview = `graph.addVertex(T.label, 'review', T.id, v, 'rid', rid, 'repo', repo, 'created_at', created, 'updated_at', updated, 'doc', doc)
1`
	getReview   = `g.V(v).hasLabel('review').values('doc').toList()`
	listReviews = `def rs = g.V().hasLabel('review').has('repo', repo).toList()
rs.sort { a, b -> b.property('created_at').orElse('') <=> a.property('created_at').orElse('') ?: a.value('rid') <=> b.value('rid') }
rs.take(limit as int).collect { it.value('doc') }`
	deleteReview = `g.V(v).hasLabel('review').drop().iterate()
1`
)

func reviewVID(id string) string { return vid("r", id) }

// SaveReview implements store.ReviewStore.
func (s *Store) SaveReview(ctx context.Context, r reviews.Review) error {
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.write(ctx, saveReview, map[string]any{"v": reviewVID(r.ID), "rid": r.ID, "repo": r.Repo, "created": r.CreatedAt,
		"updated": r.UpdatedAt, "doc": string(doc)}, nil)
}

// GetReview implements store.ReviewStore.
func (s *Store) GetReview(ctx context.Context, id string) (reviews.Review, error) {
	var docs []string
	if err := s.read(ctx, getReview, map[string]any{"v": reviewVID(id)}, &docs); err != nil {
		return reviews.Review{}, err
	}
	if len(docs) == 0 {
		return reviews.Review{}, fmt.Errorf("review %q: %w", id, store.ErrNotFound)
	}
	var r reviews.Review
	err := json.Unmarshal([]byte(docs[0]), &r)
	return r, err
}

// ListReviews implements store.ReviewStore.
func (s *Store) ListReviews(ctx context.Context, repo string, limit int) ([]reviews.Review, error) {
	var docs []string
	if err := s.read(ctx, listReviews, map[string]any{"repo": repo, "limit": store.ClampLimit(limit, 50)}, &docs); err != nil {
		return nil, err
	}
	out := make([]reviews.Review, 0, len(docs))
	for _, d := range docs {
		var r reviews.Review
		if err := json.Unmarshal([]byte(d), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// DeleteReview implements store.ReviewStore.
func (s *Store) DeleteReview(ctx context.Context, id string) error {
	return s.write(ctx, deleteReview, map[string]any{"v": reviewVID(id)}, nil)
}

const (
	// addUsage adds to existing counters by ID lookup (allowed after writes).
	addUsage = `rows.each { r ->
  def calls = r.calls as long, errors = r.errors as long, tokens = r.tokens as long, ms = r.ms as long, maxMs = r.max_ms as long
  def old = g.V(r.v).tryNext()
  if (old.isPresent()) {
    def o = old.get()
    calls += o.property('calls').orElse(0L); errors += o.property('errors').orElse(0L)
    tokens += o.property('response_tokens').orElse(0L); ms += o.property('duration_ms').orElse(0L)
    maxMs = Math.max(maxMs, o.property('max_duration_ms').orElse(0L))
  }
  graph.addVertex(T.label, 'usage', T.id, r.v, 'ukey', r.key, 'day', r.day, 'daynum', r.daynum as long, 'org', r.org, 'repo', r.repo,
    'tool', r.tool, 'client', r.client, 'calls', calls, 'errors', errors, 'response_tokens', tokens, 'duration_ms', ms, 'max_duration_ms', maxMs)
}
rows.size()`
	pruneUsage = `g.V().hasLabel('usage').has('daynum', lt(day as long)).drop().iterate()
1`
	readUsage = `g.V().hasLabel('usage').has('daynum', gte(day as long)).toList().collect { v ->
  [day: v.value('day'), org: v.property('org').orElse(''), repo: v.property('repo').orElse(''), tool: v.property('tool').orElse(''),
   client: v.property('client').orElse(''), calls: v.property('calls').orElse(0L), errors: v.property('errors').orElse(0L),
   response_tokens: v.property('response_tokens').orElse(0L), duration_ms: v.property('duration_ms').orElse(0L),
   max_duration_ms: v.property('max_duration_ms').orElse(0L)] }`
)

// dayNum turns YYYY-MM-DD into YYYYMMDD for the range index.
func dayNum(day string) (int64, error) {
	if len(day) != 10 || day[4] != '-' || day[7] != '-' {
		return 0, fmt.Errorf("invalid day %q", day)
	}
	return strconv.ParseInt(strings.ReplaceAll(day, "-", ""), 10, 64)
}

// AddUsage implements store.UsageStore. Concurrent writers to one bucket
// could lose an increment (read, then write); there is one recorder per
// MCP process and it flushes on a timer.
func (s *Store) AddUsage(ctx context.Context, buckets []usage.Bucket) error {
	if len(buckets) == 0 {
		return nil
	}
	rows := make([]map[string]any, len(buckets))
	for i, b := range buckets {
		n, err := dayNum(b.Day)
		if err != nil {
			return err
		}
		rows[i] = map[string]any{"v": vid("u", b.Key()), "key": b.Key(), "day": b.Day, "daynum": n, "org": b.Org, "repo": b.Repo,
			"tool": b.Tool, "client": b.Client, "calls": b.Calls, "errors": b.Errors, "tokens": b.ResponseTokens, "ms": b.DurationMs, "max_ms": b.MaxDurationMs}
	}
	return s.batched(ctx, addUsage, rows, nil)
}

// PruneUsage implements store.UsageStore.
func (s *Store) PruneUsage(ctx context.Context, beforeDay string) error {
	n, err := dayNum(beforeDay)
	if err != nil {
		return err
	}
	return s.write(ctx, pruneUsage, map[string]any{"day": n}, nil)
}

// Usage implements store.UsageStore.
func (s *Store) Usage(ctx context.Context, sinceDay string) ([]usage.Bucket, error) {
	n, err := dayNum(sinceDay)
	if err != nil {
		return nil, err
	}
	out := []usage.Bucket{}
	if err := s.read(ctx, readUsage, map[string]any{"day": n}, &out); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b usage.Bucket) int {
		return cmp.Or(cmp.Compare(a.Day, b.Day), cmp.Compare(a.Org, b.Org), cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Tool, b.Tool), cmp.Compare(a.Client, b.Client))
	})
	return out, nil
}
