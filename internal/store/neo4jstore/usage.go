package neo4jstore

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/usage"
)

// MCP usage counters are :McpUsage nodes (not :Node), one per day,
// repository, tool and client, so they survive re-indexing.

// AddUsage implements store.UsageStore.
func (s *Store) AddUsage(ctx context.Context, buckets []usage.Bucket) error {
	rows := make([]map[string]any, len(buckets))
	for i, b := range buckets {
		rows[i] = map[string]any{"key": b.Key(), "day": b.Day, "org": b.Org, "repo": b.Repo, "tool": b.Tool, "client": b.Client,
			"calls": b.Calls, "errors": b.Errors, "tokens": b.ResponseTokens, "ms": b.DurationMs, "max_ms": b.MaxDurationMs}
	}
	_, err := s.write(ctx, `UNWIND $rows AS r
MERGE (u:McpUsage {key: r.key})
ON CREATE SET u.day = r.day, u.org = r.org, u.repo = r.repo, u.tool = r.tool, u.client = r.client,
  u.calls = r.calls, u.errors = r.errors, u.response_tokens = r.tokens, u.duration_ms = r.ms, u.max_duration_ms = r.max_ms
ON MATCH SET u.calls = u.calls + r.calls, u.errors = u.errors + r.errors, u.response_tokens = u.response_tokens + r.tokens,
  u.duration_ms = u.duration_ms + r.ms, u.max_duration_ms = CASE WHEN r.max_ms > u.max_duration_ms THEN r.max_ms ELSE u.max_duration_ms END`,
		map[string]any{"rows": rows})
	return err
}

// PruneUsage implements store.UsageStore.
func (s *Store) PruneUsage(ctx context.Context, beforeDay string) error {
	_, err := s.write(ctx, "MATCH (u:McpUsage) WHERE u.day < $day DELETE u", map[string]any{"day": beforeDay})
	return err
}

// Usage implements store.UsageStore.
func (s *Store) Usage(ctx context.Context, sinceDay string) ([]usage.Bucket, error) {
	res, err := s.read(ctx, `MATCH (u:McpUsage) WHERE u.day >= $day
RETURN u.day AS day, coalesce(u.org, '') AS org, u.repo AS repo, u.tool AS tool, u.client AS client, u.calls AS calls, u.errors AS errors,
  u.response_tokens AS tokens, u.duration_ms AS ms, u.max_duration_ms AS max_ms
ORDER BY day, org, repo, tool, client`, map[string]any{"day": sinceDay})
	if err != nil {
		return nil, err
	}
	out := make([]usage.Bucket, 0, len(res.Records))
	for _, rec := range res.Records {
		m := rec.AsMap()
		n := func(k string) int { v, _ := m[k].(int64); return int(v) }
		out = append(out, usage.Bucket{Day: str(rec, "day"), Org: str(rec, "org"), Repo: str(rec, "repo"), Tool: str(rec, "tool"), Client: str(rec, "client"),
			Calls: n("calls"), Errors: n("errors"), ResponseTokens: n("tokens"), DurationMs: n("ms"), MaxDurationMs: n("max_ms")})
	}
	return out, nil
}
