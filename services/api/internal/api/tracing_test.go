package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
	return rec
}

// TestTraceFollowsTheRequestIntoTheWorker: the API's request span is named
// by route, never by path; its trace ID is the request ID; and the worker's
// job span continues the same trace.
func TestTraceFollowsTheRequestIntoTheWorker(t *testing.T) {
	spans := recordSpans(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	sources := t.TempDir()
	proj := filepath.Join(sources, "proj")
	require.NoError(t, os.Mkdir(proj, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "README.md"), []byte("# Proj\n"), 0o600))
	st := memory.New()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("proj", model.KindRepository, "proj"), Kind: model.KindRepository, Repo: "proj", Name: "proj", Root: proj})
	require.NoError(t, st.ReplaceRepo(ctx, "proj", g))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	u, org, err := acc.SignUp(ctx, "jobs@example.com", "Jobs", pw)
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: "proj", AddedBy: u.ID, AddedAt: time.Now()}))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := New(ctx, Config{Accounts: acc, SourcesRoot: sources, Workspace: t.TempDir(), JobRoles: []string{}}, st, logger)
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	browser := newClient(t, ts)
	require.Equal(t, browser.do("POST", "/api/v1/auth/login", `{"email":"jobs@example.com","password":"`+pw+`"}`, nil), 200)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/v1/repos/proj/sync", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(OrgHeader, org.Slug)
	// An outside caller's traceparent is not continued.
	req.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	res, err := browser.http.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.Equal(t, res.StatusCode, http.StatusAccepted)
	traceID := res.Header.Get("X-Request-ID")
	assert.Equal(t, len(traceID), 32, "the request ID is the trace ID")
	assert.True(t, traceID != "11111111111111111111111111111111", "an untrusted traceparent is ignored")

	_ = New(ctx, Config{Accounts: acc, SourcesRoot: sources, Workspace: t.TempDir(), JobRoles: []string{"ingest"}, Worker: true}, st, logger)
	deadline := time.Now().Add(30 * time.Second)
	var job sdktrace.ReadOnlySpan
	for job == nil && time.Now().Before(deadline) {
		for _, s := range spans.Ended() {
			if s.Name() == "job sync" {
				job = s
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, job != nil, "the worker traced the job")
	assert.Equal(t, job.SpanContext().TraceID().String(), traceID, "in the request's trace")

	names := map[string]bool{}
	for _, s := range spans.Ended() {
		names[s.Name()] = true
		for _, a := range s.Attributes() {
			assert.False(t, strings.Contains(a.Value.String(), "proj"), "no repository names in spans: "+s.Name()+" "+string(a.Key))
		}
		assert.False(t, strings.Contains(s.Name(), "proj"), "span names carry route patterns: "+s.Name())
	}
	assert.True(t, names["POST /api/v1/repos/{repo}/sync"], "named by route")
	assert.True(t, names["job.ingest"] && names["job.publish"], "job phases")

	// The dashboard proxy is trusted: its traceparent is continued.
	api.cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/auth/status", nil)
	require.NoError(t, err)
	req.Header.Set("traceparent", "00-33333333333333333333333333333333-4444444444444444-01")
	res, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	assert.Equal(t, res.Header.Get("X-Request-ID"), "33333333333333333333333333333333")
}
