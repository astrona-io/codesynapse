package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestHistogramTenantShare(t *testing.T) {
	r := NewRegistry()
	h := r.ScopedHistogram("wait_seconds", "Wait.", slow, "kind").TenantShare(2)
	a := WithTenant(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaa")
	b := WithTenant(context.Background(), "bbbbbbbbbbbbbbbbbbbbbbbb")
	for i := range 5 {
		h.ObserveContext(a, 1, fmt.Sprintf("k%d", i))
	}
	h.ObserveContext(a, 1, "k0")
	h.ObserveContext(b, 1, "k0")
	assert.Equal(t, len(h.vals), 3, "the first tenant keeps two series, the second still gets one")
	assert.Equal(t, r.dropped.vals["wait_seconds"], 3.0, "each refused combination is counted")
	out := text(t, r)
	assert.Contains(t, out, `kind="k1",tenant_id="aaaaaaaaaaaaaaaaaaaaaaaa"`)
	assert.False(t, strings.Contains(out, `kind="k2"`))
	assert.Contains(t, out, `kind="k0",tenant_id="bbbbbbbbbbbbbbbbbbbbbbbb"`)
}

func TestJobMetricsStayInTheirSets(t *testing.T) {
	ctx := WithTenant(context.Background(), "cccccccccccccccccccccccc")
	RecordJobFinished(ctx, "sync", JobError)
	RecordJobFinished(ctx, "/etc/passwd", JobOK)
	RecordJobFinished(ctx, "sync", "exploded")
	RecordJobWait(ctx, "fix", -time.Second)
	RecordJobDuration(ctx, "ai-auto", 2*time.Minute)
	RecordIngestPhase(ctx, PhasePublish, time.Second, false)
	RecordIngestPhase(ctx, "job.publish", time.Second, true)
	RecordExtract(ctx, "typescript", time.Second)
	RecordExtract(ctx, "cobol", time.Second)
	RecordJobFinished(context.Background(), "clone", JobLeftBehind)

	out := text(t, Default)
	const id = `tenant_id="cccccccccccccccccccccccc"`
	assert.Contains(t, out, `codesynapse_jobs_finished_total{kind="sync",outcome="error",`+id)
	assert.Contains(t, out, `codesynapse_jobs_finished_total{kind="other",outcome="ok",`+id)
	assert.False(t, strings.Contains(out, "passwd"), "kinds come from a fixed set")
	assert.False(t, strings.Contains(out, "exploded"), "unknown outcomes are not recorded")
	assert.Contains(t, out, `codesynapse_job_wait_seconds_bucket{kind="fix",`+id)
	assert.Contains(t, out, `codesynapse_job_duration_seconds_count{kind="ai-auto",`+id)
	assert.Contains(t, out, `codesynapse_ingest_phase_total{phase="publish",outcome="error",`+id)
	assert.False(t, strings.Contains(out, `phase="job.publish"`))
	assert.Contains(t, out, `codesynapse_ingest_extract_seconds_total{language="jsts",`+id)
	assert.Contains(t, out, `codesynapse_ingest_extract_seconds_total{language="other",`+id)
	assert.False(t, strings.Contains(out, "cobol"))
	assert.Contains(t, out, `platform_codesynapse_jobs_finished_total{kind="clone",outcome="left_behind",`)
}
