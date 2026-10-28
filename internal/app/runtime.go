package app

import (
	"context"
	"math"
	"runtime/metrics"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// runtimeEvery is how often process health is sampled; it matches the
// export interval, so each export carries a fresh value.
const runtimeEvery = 15 * time.Second

// runtime/metrics names (stable since Go 1.22).
const (
	rmGoroutines = "/sched/goroutines:goroutines"
	rmHeap       = "/memory/classes/heap/objects:bytes"
	rmGCPauses   = "/sched/pauses/total/gc:seconds"
	rmCPUTotal   = "/cpu/classes/total:cpu-seconds"
	rmCPUIdle    = "/cpu/classes/idle:cpu-seconds"
)

// runtimeSampler turns the standard library's runtime/metrics and the
// tracked connection pools into the platform health instruments. The
// runtime reports cumulative values; counters receive the increase since
// the previous sample, so the registry stays the only accumulator.
type runtimeSampler struct {
	samples []metrics.Sample
	prev    map[string]float64
}

func newRuntimeSampler() *runtimeSampler {
	names := []string{rmGoroutines, rmHeap, rmGCPauses, rmCPUTotal, rmCPUIdle}
	s := &runtimeSampler{prev: map[string]float64{}}
	for _, n := range names {
		s.samples = append(s.samples, metrics.Sample{Name: n})
	}
	return s
}

// runRuntimeMetrics samples until ctx ends, once at the start.
func runRuntimeMetrics(ctx context.Context) {
	s := newRuntimeSampler()
	t := time.NewTicker(runtimeEvery)
	defer t.Stop()
	for {
		s.sample(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *runtimeSampler) sample(ctx context.Context) {
	metrics.Read(s.samples)
	v := map[string]float64{}
	for _, m := range s.samples {
		switch m.Value.Kind() {
		case metrics.KindUint64:
			v[m.Name] = float64(m.Value.Uint64())
		case metrics.KindFloat64:
			v[m.Name] = m.Value.Float64()
		case metrics.KindFloat64Histogram:
			v[m.Name] = histogramSum(m.Value.Float64Histogram())
		}
	}
	pools := telemetry.PoolStats()
	telemetry.Default.UpdateGauges(func() {
		if g, ok := v[rmGoroutines]; ok {
			telemetry.GoGoroutines.SetContext(ctx, g)
		}
		if h, ok := v[rmHeap]; ok {
			telemetry.GoHeapBytes.SetContext(ctx, h)
		}
		for system, st := range pools {
			telemetry.DBPoolConnections.SetContext(ctx, float64(st.Idle), system, "idle")
			telemetry.DBPoolConnections.SetContext(ctx, float64(st.InUse), system, "in_use")
			telemetry.DBPoolConnections.SetContext(ctx, float64(st.Max), system, "max")
		}
	})
	if p, ok := v[rmGCPauses]; ok {
		telemetry.GoGCPauseSeconds.AddContext(ctx, s.increase("gc", p))
	}
	total, okT := v[rmCPUTotal]
	idle, okI := v[rmCPUIdle]
	if okT && okI {
		telemetry.ProcessCPUSeconds.AddContext(ctx, s.increase("cpu", total-idle))
	}
	for system, st := range pools {
		telemetry.DBPoolAcquireWaits.AddContext(ctx, s.increase("waits/"+system, float64(st.EmptyAcquires)), system)
		telemetry.DBPoolAcquireWaitSeconds.AddContext(ctx, s.increase("wait/"+system, st.EmptyAcquireWait.Seconds()), system)
	}
}

// increase is how much a cumulative value grew since the last sample. A
// value that fell (a pool closed) restarts from there and adds nothing.
func (s *runtimeSampler) increase(key string, v float64) float64 {
	prev, seen := s.prev[key]
	s.prev[key] = v
	if !seen {
		// The first sample counts everything since the process started.
		return max(v, 0)
	}
	return max(v-prev, 0)
}

// histogramSum estimates the total of a runtime histogram from its bucket
// midpoints; an open-ended bucket uses its finite edge.
func histogramSum(h *metrics.Float64Histogram) float64 {
	if h == nil {
		return 0
	}
	var sum float64
	for i, n := range h.Counts {
		if n == 0 || i+1 >= len(h.Buckets) {
			continue
		}
		lo, hi := h.Buckets[i], h.Buckets[i+1]
		var mid float64
		switch {
		case math.IsInf(lo, -1) && math.IsInf(hi, 1):
			continue
		case math.IsInf(lo, -1):
			mid = hi
		case math.IsInf(hi, 1):
			mid = lo
		default:
			mid = (lo + hi) / 2
		}
		sum += float64(n) * mid
	}
	return sum
}
