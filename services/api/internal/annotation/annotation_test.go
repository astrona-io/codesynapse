package annotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

func batch() Batch {
	return Batch{Graph: "repo", Commit: "commit", Revision: "revision", PolicyDigest: "digest", MaxBatchSize: 20, Symbols: []Symbol{{ID: "symbol", Name: "Check", Signature: "func Check() error", Summary: "Checks a request.", Fingerprint: "fingerprint"}}, TaxonomyHints: []string{"auth"}}
}

func identityContext(t *testing.T, tenant string) context.Context {
	t.Helper()
	ctx, err := execution.With(t.Context(), execution.Identity{TenantID: tenant, SubjectID: "user", SessionID: "thread", ParentSessionID: "thread"})
	require.NoError(t, err)
	return ctx
}

func registry(url string) *llm.Registry {
	return llm.NewRegistry(llm.Config{Default: "local", Providers: []llm.ProviderConfig{{Name: "local", Kind: llm.KindOpenAI, BaseURL: url, Model: "model", MaxInputTokens: 30000, MaxOutputTokens: 8192, TimeoutSeconds: 10}, {Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://example.com", Model: "remote"}}})
}

const validAnswer = `{"items":[{"symbol_id":"symbol","summary":"Checks a request.","taxonomy":["auth"],"confidence":0.9}]}`

func fakeModel(t *testing.T, handle func(*http.Request) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text := handle(r)
		b, _ := json.Marshal(text)
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%s},"finish_reason":"stop"}],"model":"model"}`, b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func allow(context.Context) error            { return nil }
func unchanged(context.Context, Batch) error { return nil }

func waitDone(ctx context.Context, t *testing.T, w *Worker, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := w.Get(ctx, id)
		require.True(t, ok)
		if j.State != Queued && j.State != Running {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("annotation did not finish")
	return Job{}
}

func runWorker(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestAnnotationCarriesIdentityAndDeduplicates(t *testing.T) {
	var calls atomic.Int32
	srv := fakeModel(t, func(r *http.Request) string {
		calls.Add(1)
		var input struct{ Messages []struct{ Content string } }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		require.Len(t, input.Messages, 2)
		assert.Equal(t, input.Messages[0].Content, system)
		assert.True(t, strings.Contains(input.Messages[1].Content, "func Check() error"))
		return validAnswer
	})
	saved := make(chan Result, 1)
	w := New(registry(srv.URL), func(ctx context.Context, result Result) error {
		id, err := execution.Require(ctx)
		require.NoError(t, err)
		assert.Equal(t, id, result.Job.Execution)
		saved <- result
		return nil
	})
	ctx, cancel := context.WithCancel(identityContext(t, "tenant"))
	j, err := w.Submit(ctx, batch(), allow, unchanged)
	require.NoError(t, err)
	again, err := w.Submit(ctx, batch(), allow, unchanged)
	require.NoError(t, err)
	assert.Equal(t, j.ID, again.ID)
	cancel()
	runWorker(t, w)
	readCtx := identityContext(t, "tenant")
	done := waitDone(readCtx, t, w, j.ID)
	assert.Equal(t, done.State, Succeeded, done.Error)
	result := <-saved
	assert.Len(t, result.Items, 1)
	assert.Equal(t, result.Job.State, Succeeded)
	assert.Equal(t, calls.Load(), int32(1))
	_, ok := w.Get(identityContext(t, "other"), j.ID)
	assert.False(t, ok)
	assert.False(t, w.Cancel(identityContext(t, "other"), j.ID))
	w.mu.Lock()
	assert.Len(t, w.jobs[j.ID].batch.Symbols, 0)
	assert.True(t, w.jobs[j.ID].authorize == nil)
	w.mu.Unlock()
}

func TestRechecksRevocationAndRevisionBeforePersistence(t *testing.T) {
	for _, stale := range []string{"permission", "revision"} {
		t.Run(stale, func(t *testing.T) {
			var changed atomic.Bool
			srv := fakeModel(t, func(*http.Request) string { changed.Store(true); return validAnswer })
			var writes atomic.Int32
			w := New(registry(srv.URL), func(context.Context, Result) error { writes.Add(1); return nil })
			authorize := func(context.Context) error {
				if stale == "permission" && changed.Load() {
					return errors.New("revoked")
				}
				return nil
			}
			current := func(context.Context, Batch) error {
				if stale == "revision" && changed.Load() {
					return errors.New("changed")
				}
				return nil
			}
			ctx := identityContext(t, "tenant")
			j, err := w.Submit(ctx, batch(), authorize, current)
			require.NoError(t, err)
			runWorker(t, w)
			assert.Equal(t, waitDone(ctx, t, w, j.ID).State, Failed)
			assert.Equal(t, writes.Load(), int32(0))
		})
	}
}

func TestQueueLimitsAndShutdown(t *testing.T) {
	w := New(registry("http://127.0.0.1:1"), func(context.Context, Result) error { return nil })
	ctx := identityContext(t, "tenant")
	for i := range MaxTenantPending {
		b := batch()
		b.Revision = fmt.Sprint(i)
		_, err := w.Submit(ctx, b, allow, unchanged)
		require.NoError(t, err)
	}
	b := batch()
	b.Revision = "overflow"
	_, err := w.Submit(ctx, b, allow, unchanged)
	assert.True(t, errors.Is(err, ErrBusy))
	other, err := w.Submit(identityContext(t, "other"), b, allow, unchanged)
	require.NoError(t, err)
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	w.Run(stopped)
	got, ok := w.Get(identityContext(t, "other"), other.ID)
	require.True(t, ok)
	assert.Equal(t, got.State, Cancelled)
	_, err = w.Submit(ctx, batch(), allow, unchanged)
	assert.True(t, errors.Is(err, ErrClosed))
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.jobs {
		assert.Len(t, p.batch.Symbols, 0)
		assert.True(t, p.authorize == nil)
	}
}

func TestSubmissionRequiresLocalProviderAndChecks(t *testing.T) {
	reg := registry("http://127.0.0.1:1")
	w := New(reg, func(context.Context, Result) error { return nil })
	_, err := w.Submit(t.Context(), batch(), allow, unchanged)
	assert.True(t, errors.Is(err, execution.ErrMissing))
	ctx := identityContext(t, "tenant")
	_, err = w.Submit(ctx, batch(), nil, unchanged)
	assert.True(t, errors.Is(err, ErrAuthorization))
	require.NoError(t, reg.SetRoutes(llm.Routes{Default: "cloud"}))
	_, err = w.Submit(ctx, batch(), allow, unchanged)
	assert.True(t, errors.Is(err, ErrLocal))
}

func TestModelOutputValidation(t *testing.T) {
	for _, tc := range []struct{ name, answer string }{
		{"foreign", strings.ReplaceAll(validAnswer, `"symbol"`, `"foreign"`)},
		{"unknown_field", strings.Replace(validAnswer, `"summary":`, `"unknown":"x","summary":`, 1)},
		{"unknown_taxonomy", strings.ReplaceAll(validAnswer, `"auth"`, `"admin"`)},
		{"confidence", strings.Replace(validAnswer, "0.9", "1.1", 1)},
		{"duplicate_field", strings.Replace(validAnswer, `"symbol_id":`, `"symbol_id":"foreign","symbol_id":`, 1)},
		{"duplicate_symbol", `{"items":[{"symbol_id":"symbol","summary":"a","taxonomy":[]},{"symbol_id":"symbol","summary":"b","taxonomy":[]}]}`},
		{"prose", "prefix " + validAnswer},
		{"trailing", validAnswer + "{}"},
		{"oversized", strings.Repeat(" ", MaxOutputBytes) + validAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) { _, err := parse(tc.answer, batch()); assert.True(t, errors.Is(err, ErrInvalid)) })
	}
	items, err := parse(validAnswer, batch())
	require.NoError(t, err)
	assert.Len(t, items, 1)
}

func TestCancelBeforeWork(t *testing.T) {
	w := New(registry("http://127.0.0.1:1"), func(context.Context, Result) error { t.Error("cancelled job persisted"); return nil })
	ctx := identityContext(t, "tenant")
	j, err := w.Submit(ctx, batch(), allow, unchanged)
	require.NoError(t, err)
	assert.True(t, w.Cancel(ctx, j.ID))
	w.run(t.Context(), j.ID)
	got, ok := w.Get(ctx, j.ID)
	require.True(t, ok)
	assert.Equal(t, got.State, Cancelled)
}

func FuzzParse(f *testing.F) {
	f.Add(validAnswer)
	f.Add(`{"items":[]}`)
	f.Fuzz(func(_ *testing.T, answer string) { _, _ = parse(answer, batch()) })
}
