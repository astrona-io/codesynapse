package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

type fakeSink struct {
	got    []Bucket
	pruned string
	fail   bool
}

func (f *fakeSink) AddUsage(_ context.Context, b []Bucket) error {
	if f.fail {
		return errors.New("down")
	}
	f.got = append(f.got, b...)
	return nil
}

func (f *fakeSink) PruneUsage(_ context.Context, before string) error { f.pruned = before; return nil }

func TestRecorder(t *testing.T) {
	sink := &fakeSink{fail: true}
	r := NewRecorder(sink)
	r.now = func() time.Time { return time.Date(2026, 10, 22, 23, 0, 0, 0, time.UTC) }
	r.Record("repo", "search", "claude-code", 20*time.Millisecond, 100, false)
	r.Record("repo", "search", "claude-code", 50*time.Millisecond, 50, true)
	r.Record("", Session, "", 0, 0, false)

	require.Error(t, r.Flush(context.Background()))
	r.Record("repo", "search", "claude-code", 10*time.Millisecond, 10, false)
	sink.fail = false
	require.NoError(t, r.Flush(context.Background()))
	require.Len(t, sink.got, 2, "a failed flush keeps its buckets")
	byTool := map[string]Bucket{}
	for _, b := range sink.got {
		byTool[b.Tool] = b
	}
	assert.Equal(t, byTool["search"], Bucket{Day: "2026-10-22", Repo: "repo", Tool: "search", Client: "claude-code", Calls: 3, Errors: 1,
		ResponseTokens: 160, DurationMs: 80, MaxDurationMs: 50})
	assert.Equal(t, byTool[Session].Client, "unknown")
	assert.Equal(t, sink.pruned, "2026-04-25", "180 days kept")
	require.NoError(t, r.Flush(context.Background()), "nothing pending is fine")
}

func TestRecorderBoundsAndCleans(t *testing.T) {
	r := NewRecorder(&fakeSink{})
	for i := range maxBuckets + 10 {
		r.Record(string(rune('a'+i%26))+time.Duration(i).String(), "t", "c", 0, 0, false)
	}
	assert.Equal(t, len(r.pending), maxBuckets)
	assert.Equal(t, clean(" evil\x1b[31mclient\n ", 64), "evil[31mclient")
}
