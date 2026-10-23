package milvus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fakeMilvus answers each call with the next of answers (status, body),
// repeating the last one.
func fakeMilvus(t *testing.T, answers ...[2]any) (*Index, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		a := answers[min(int(calls.Add(1))-1, len(answers)-1)]
		w.WriteHeader(a[0].(int))
		_, _ = w.Write([]byte(a[1].(string))) //nolint:errcheck // test server
	}))
	t.Cleanup(ts.Close)
	return &Index{base: ts.URL, http: ts.Client(), ready: map[string]bool{}}, calls
}

func TestCallRetriesTransientFailures(t *testing.T) {
	ok := [2]any{200, `{"code":0,"data":["a"]}`}
	for _, tc := range []struct {
		name    string
		answers [][2]any
		calls   int64
		fails   bool
	}{
		{"gateway error then ok", [][2]any{{503, `upstream`}, {502, `{"code":0}`}, ok}, 3, false},
		{"not ready then ok", [][2]any{{200, `{"code":1,"message":"service not ready"}`}, {200, `{"code":503,"message":"channel not available"}`}, ok}, 3, false},
		{"bad request is final", [][2]any{{200, `{"code":1100,"message":"invalid parameter"}`}, ok}, 1, true},
		{"missing collection is final here", [][2]any{{200, `{"code":100,"message":"collection not found"}`}, ok}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, calls := fakeMilvus(t, tc.answers...)
			var names []string
			err := x.call(t.Context(), "collections/list", map[string]any{}, &names)
			assert.Equal(t, calls.Load(), tc.calls)
			assert.Equal(t, err != nil, tc.fails)
			if !tc.fails {
				assert.Equal(t, names, []string{"a"})
			}
		})
	}
}

func TestCallStopsRetryingWithTheContext(t *testing.T) {
	x, calls := fakeMilvus(t, [2]any{503, `down`})
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := x.call(ctx, "collections/list", map[string]any{}, nil)
	require.Error(t, err)
	assert.True(t, time.Since(start) < 2*time.Second, "the caller's deadline wins over the retry budget")
	assert.True(t, calls.Load() >= 2, "retried before the deadline")
}

func TestCallRetriesRefusedConnections(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	x := &Index{base: url, http: &http.Client{Timeout: time.Second}, ready: map[string]bool{}}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	err := x.call(ctx, "collections/list", map[string]any{}, nil)
	require.Error(t, err)
	assert.True(t, transient(err) || ctx.Err() != nil, "a refused connection is transient")
}
