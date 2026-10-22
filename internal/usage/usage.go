// Package usage counts how agents use the MCP server, aggregated per day,
// repository, tool and client: calls, errors, response size and latency.
// Arguments and results are never recorded, only their size.
package usage

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Session is the pseudo tool under which new MCP sessions are counted.
const Session = "(session)"

// Retention is how long daily buckets are kept.
const Retention = 180 * 24 * time.Hour

// maxBuckets bounds the buckets held between flushes; a flood of distinct
// repositories or client names cannot grow memory without limit.
const maxBuckets = 5000

// Bucket is one day of usage for a (repository, tool, client) triple.
type Bucket struct {
	Day string `json:"day"` // YYYY-MM-DD, UTC
	// Org is the organisation of calls made with personal API keys ("" for
	// the MCP token and single-tenant installs).
	Org    string `json:"org,omitempty"`
	Repo   string `json:"repo"`
	Tool   string `json:"tool"`
	Client string `json:"client"`
	Calls  int    `json:"calls"`
	Errors int    `json:"errors"`
	// ResponseTokens estimates the tokens returned to the agent (bytes/4).
	ResponseTokens int `json:"response_tokens"`
	DurationMs     int `json:"duration_ms"`
	MaxDurationMs  int `json:"max_duration_ms"`
}

// Key identifies a bucket.
func (b Bucket) Key() string {
	return b.Day + "|" + b.Org + "|" + b.Repo + "|" + b.Tool + "|" + b.Client
}

// Add merges o's counters into b.
func (b *Bucket) Add(o Bucket) {
	b.Calls += o.Calls
	b.Errors += o.Errors
	b.ResponseTokens += o.ResponseTokens
	b.DurationMs += o.DurationMs
	b.MaxDurationMs = max(b.MaxDurationMs, o.MaxDurationMs)
}

// Day formats t as a bucket day.
func Day(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// Sink persists buckets by adding them to what is stored.
type Sink interface {
	AddUsage(ctx context.Context, buckets []Bucket) error
	PruneUsage(ctx context.Context, beforeDay string) error
}

// Recorder aggregates usage in memory and flushes it to a Sink. It is safe
// for concurrent use; the zero value is not, use NewRecorder.
type Recorder struct {
	mu      sync.Mutex
	pending map[string]*Bucket
	sink    Sink
	now     func() time.Time
}

// NewRecorder returns a recorder writing to sink.
func NewRecorder(sink Sink) *Recorder {
	return &Recorder{pending: map[string]*Bucket{}, sink: sink, now: time.Now}
}

// Record counts one call. tokens is the response size estimate; failed marks
// tool or protocol errors.
func (r *Recorder) Record(org, repo, tool, client string, d time.Duration, tokens int, failed bool) {
	b := Bucket{Day: Day(r.now()), Org: clean(org, 64), Repo: clean(repo, 200), Tool: clean(tool, 64), Client: clean(client, 64), Calls: 1,
		ResponseTokens: tokens, DurationMs: int(d.Milliseconds()), MaxDurationMs: int(d.Milliseconds())}
	if failed {
		b.Errors = 1
	}
	if b.Client == "" {
		b.Client = "unknown"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.pending[b.Key()]; ok {
		cur.Add(b)
		return
	}
	if len(r.pending) >= maxBuckets {
		return // dropped until the next flush; counters are best effort
	}
	r.pending[b.Key()] = &b
}

// Flush writes pending buckets and prunes expired ones. On failure the
// buckets are kept for the next attempt.
func (r *Recorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	batch := make([]Bucket, 0, len(r.pending))
	for _, b := range r.pending {
		batch = append(batch, *b)
	}
	r.pending = map[string]*Bucket{}
	r.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	if err := r.sink.AddUsage(ctx, batch); err != nil {
		r.mu.Lock()
		for _, b := range batch {
			if cur, ok := r.pending[b.Key()]; ok {
				cur.Add(b)
			} else if len(r.pending) < maxBuckets {
				r.pending[b.Key()] = &b
			}
		}
		r.mu.Unlock()
		return err
	}
	return r.sink.PruneUsage(ctx, Day(r.now().Add(-Retention)))
}

// Run flushes every interval until ctx ends, then once more with a short
// grace period so a clean shutdown keeps the last counts.
func (r *Recorder) Run(ctx context.Context, interval time.Duration, onErr func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := r.Flush(flushCtx); err != nil && onErr != nil {
				onErr(err)
			}
			cancel()
			return
		case <-t.C:
			if err := r.Flush(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// clean trims, drops control characters and caps length: client names come
// from the client and are shown in the UI.
func clean(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > limit {
		return string(r[:limit])
	}
	return s
}
