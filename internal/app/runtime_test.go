package app

import (
	"bytes"
	"context"
	"math"
	"regexp"
	"runtime"
	"runtime/metrics"
	"strconv"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// sampleValue reads one unlabelled sample from the default registry's text.
func sampleValue(t *testing.T, line string) float64 {
	t.Helper()
	var text bytes.Buffer
	require.NoError(t, telemetry.Default.WriteText(&text))
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(line) + ` (\S+)$`).FindStringSubmatch(text.String())
	require.True(t, m != nil, line+" is exported")
	v, err := strconv.ParseFloat(m[1], 64)
	require.NoError(t, err)
	return v
}

func TestRuntimeSamplerRecordsProcessHealth(t *testing.T) {
	untrack := telemetry.TrackPool(telemetry.DBSystemPostgres, func() telemetry.PoolStat {
		return telemetry.PoolStat{Idle: 2, InUse: 3, Max: 10, EmptyAcquires: 7, EmptyAcquireWait: 2 * time.Second}
	})
	defer untrack()
	s := newRuntimeSampler()
	runtime.GC()
	s.sample(context.Background())
	assert.True(t, sampleValue(t, "platform_go_goroutines") >= 1)
	assert.True(t, sampleValue(t, "platform_go_heap_bytes") > 0)
	assert.Equal(t, sampleValue(t, `platform_db_pool_connections{db_system="postgres",state="in_use"}`), 3.0)
	assert.Equal(t, sampleValue(t, `platform_db_pool_connections{db_system="postgres",state="max"}`), 10.0)
	waits := sampleValue(t, `platform_db_pool_acquire_waits_total{db_system="postgres"}`)
	// A second sample of the same cumulative value adds nothing.
	s.sample(context.Background())
	assert.Equal(t, sampleValue(t, `platform_db_pool_acquire_waits_total{db_system="postgres"}`), waits)
	cpu := sampleValue(t, "platform_process_cpu_seconds_total")
	assert.True(t, cpu >= 0)
}

func TestIncreaseNeverGoesBackwards(t *testing.T) {
	s := newRuntimeSampler()
	assert.Equal(t, s.increase("k", 5), 5.0, "the first sample counts from process start")
	assert.Equal(t, s.increase("k", 8), 3.0)
	assert.Equal(t, s.increase("k", 2), 0.0, "a closed pool lowers the sum: nothing added")
	assert.Equal(t, s.increase("k", 4), 2.0, "and counting continues from there")
}

func TestHistogramSum(t *testing.T) {
	h := &metrics.Float64Histogram{Counts: []uint64{1, 2, 1}, Buckets: []float64{math.Inf(-1), 1, 3, math.Inf(1)}}
	assert.Equal(t, histogramSum(h), 1*1.0+2*2.0+1*3.0)
	assert.Equal(t, histogramSum(nil), 0.0)
}
