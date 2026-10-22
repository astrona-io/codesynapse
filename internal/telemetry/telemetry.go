// Package telemetry records bounded metrics and pushes them to VictoriaMetrics.
//
// Labels stay low-cardinality by construction: every label value passes a
// per-label rule (tenant IDs must look like organisation IDs, tools must be
// registered, free text is never accepted), each metric caps its series,
// and prompt text, queries, file paths and user IDs never become labels.
// They belong in logs, not time series.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NoTenant labels work done without an organisation (single-tenant
// installs, the service token, background jobs on shared code).
const NoTenant = "none"

// maxSeries caps the label combinations of one metric; past it, new
// combinations are dropped and counted in telemetry_dropped_series_total.
const maxSeries = 5000

var tenantRe = regexp.MustCompile(`^[0-9a-f]{24}$`)

// TenantLabel turns an organisation ID into a tenant_id value. Only IDs of
// the accounts format pass; anything else is "invalid", never echoed.
func TenantLabel(org string) string {
	switch {
	case org == "":
		return NoTenant
	case tenantRe.MatchString(org):
		return org
	}
	return "invalid"
}

type tenantKey struct{}

// WithTenant records the organisation work in ctx is done for.
func WithTenant(ctx context.Context, org string) context.Context {
	return context.WithValue(ctx, tenantKey{}, TenantLabel(org))
}

// Tenant is the tenant_id label for ctx.
func Tenant(ctx context.Context) string {
	if t, ok := ctx.Value(tenantKey{}).(string); ok {
		return t
	}
	return NoTenant
}

// Registry holds metrics in registration order.
type Registry struct {
	gaugesMu sync.RWMutex
	mu       sync.Mutex
	metrics  []writer
	dropped  *Counter
}

type writer interface {
	write(w io.Writer) error
}

// NewRegistry returns an empty registry with its own drop counter.
func NewRegistry() *Registry {
	r := &Registry{}
	r.dropped = r.Counter("telemetry_dropped_series_total", "Label combinations dropped because a metric reached its series cap.", "metric")
	return r
}

// Counter registers a monotonically increasing counter.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	c := &Counter{family: newFamily(r, name, help, "counter", labels)}
	r.add(c)
	return c
}

// Histogram registers a histogram with the given upper bounds.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	b := slices.Clone(buckets)
	slices.Sort(b)
	h := &Histogram{family: newFamily(r, name, help, "histogram", labels), buckets: b}
	r.add(h)
	return h
}

func (r *Registry) add(m writer) {
	r.mu.Lock()
	r.metrics = append(r.metrics, m)
	r.mu.Unlock()
}

// WriteText writes every metric in the Prometheus text format.
func (r *Registry) WriteText(w io.Writer) error {
	r.gaugesMu.RLock()
	defer r.gaugesMu.RUnlock()
	r.mu.Lock()
	ms := slices.Clone(r.metrics)
	r.mu.Unlock()
	for _, m := range ms {
		if err := m.write(w); err != nil {
			return err
		}
	}
	return nil
}

type family struct {
	reg         *Registry
	name, help  string
	kind        string
	labels      []string
	inputLabels []string
	scoped      bool
	budget      int
}

var nameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func newFamily(r *Registry, name, help, kind string, labels []string) family {
	if !nameRe.MatchString(name) {
		panic("telemetry: invalid metric name " + name) // programmer error
	}
	for _, l := range labels {
		if !nameRe.MatchString(l) {
			panic("telemetry: invalid label name " + l) // programmer error
		}
	}
	return family{reg: r, name: name, help: help, kind: kind, labels: labels, inputLabels: labels, budget: maxSeries}
}

// key joins label values; values never contain \xff (labelValue strips it).
func (f *family) key(values []string) (string, bool) {
	if len(values) != len(f.labels) {
		return "", false
	}
	for i, v := range values {
		values[i] = labelValue(v)
	}
	return strings.Join(values, "\xff"), true
}

// labelValue bounds a value: 64 bytes of printable text.
func labelValue(v string) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '\xff' {
			return -1
		}
		return r
	}, v)
	if len(v) > 64 {
		v = v[:64]
	}
	if v == "" {
		return "none"
	}
	return v
}

func (f *family) header(w io.Writer) error {
	_, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
	if err == nil && f.scoped {
		_, err = fmt.Fprintf(w, "# HELP platform_%s %s\n# TYPE platform_%s %s\n", f.name, f.help, f.name, f.kind)
	}
	return err
}

// labelSet renders {a="x",b="y"} plus extra pairs.
func (f *family) labelSet(key string, extra ...string) string {
	var b strings.Builder
	vals := []string{}
	if len(f.labels) > 0 {
		vals = strings.Split(key, "\xff")
	}
	pairs := make([]string, 0, len(vals)+len(extra)/2)
	for i, l := range f.labels {
		if f.scoped && l == "tenant_id" && !tenantRe.MatchString(vals[i]) {
			continue
		}
		pairs = append(pairs, l+`="`+escape(vals[i])+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		pairs = append(pairs, extra[i]+`="`+escape(extra[i+1])+`"`)
	}
	if len(pairs) == 0 {
		return ""
	}
	b.WriteString("{")
	b.WriteString(strings.Join(pairs, ","))
	b.WriteString("}")
	return b.String()
}

func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func num(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// born remembers when a series appeared until its first write, which adds
// a 0 sample at that time. Without it, VictoriaMetrics' increase() takes a
// series whose first value is large (one call returning 37k tokens) for a
// counter that ran before collection started, and counts nothing.
type born map[string]int64

func (b born) note(key string) {
	if _, ok := b[key]; !ok {
		b[key] = nowMillis()
	}
}

var nowMillis = func() int64 { return time.Now().UnixMilli() }

// Counter is a counter family.
type Counter struct {
	family
	mu      sync.Mutex
	vals    map[string]float64
	fresh   born
	started born
}

// Add adds v (negative values are ignored) to the series of values.
func (c *Counter) Add(v float64, values ...string) { c.add(legacyContext, v, values...) }

// AddContext records trusted request attribution with the observation.
func (c *Counter) AddContext(ctx context.Context, v float64, values ...string) {
	c.add(ctx, v, values...)
}

// IncContext adds one with trusted request attribution.
func (c *Counter) IncContext(ctx context.Context, values ...string) { c.add(ctx, 1, values...) }

func (c *Counter) add(ctx context.Context, v float64, values ...string) {
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	key, ok := c.contextKey(ctx, values)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.vals == nil {
		c.vals, c.fresh, c.started = map[string]float64{}, born{}, born{}
	}
	if _, seen := c.vals[key]; !seen {
		if len(c.vals) >= c.budget {
			if c.reg.dropped != c {
				c.reg.dropped.AddContext(ctx, 1, c.name)
			}
			return
		}
		c.fresh.note(key)
		c.started.note(key)
	}
	c.vals[key] += v
}

// Inc adds one.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

func (c *Counter) write(w io.Writer) error {
	c.mu.Lock()
	keys := make([]string, 0, len(c.vals))
	for k := range c.vals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		if at, ok := c.fresh[k]; ok {
			lines = append(lines, c.sampleName(k)+c.labelSet(k)+" 0 "+strconv.FormatInt(at, 10)+"\n")
			delete(c.fresh, k)
		}
		lines = append(lines, c.sampleName(k)+c.labelSet(k)+" "+num(c.vals[k])+"\n")
	}
	c.mu.Unlock()
	if len(lines) == 0 {
		return nil
	}
	if err := c.header(w); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.Join(lines, ""))
	return err
}

// Histogram is a histogram family.
type Histogram struct {
	family
	buckets []float64
	mu      sync.Mutex
	vals    map[string]*hist
	fresh   born
	started born
}

type hist struct {
	counts []uint64 // per bucket, not cumulative
	sum    float64
	count  uint64
}

// Observe records v in the series of values.
func (h *Histogram) Observe(v float64, values ...string) { h.observe(legacyContext, v, values...) }

// ObserveContext records a measurement with trusted request attribution.
func (h *Histogram) ObserveContext(ctx context.Context, v float64, values ...string) {
	h.observe(ctx, v, values...)
}

func (h *Histogram) observe(ctx context.Context, v float64, values ...string) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return
	}
	key, ok := h.contextKey(ctx, values)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.vals == nil {
		h.vals, h.fresh, h.started = map[string]*hist{}, born{}, born{}
	}
	s := h.vals[key]
	if s == nil {
		if len(h.vals) >= h.budget {
			h.reg.dropped.AddContext(ctx, 1, h.name)
			return
		}
		s = &hist{counts: make([]uint64, len(h.buckets))}
		h.vals[key] = s
		h.fresh.note(key)
		h.started.note(key)
	}
	if i, _ := slices.BinarySearch(h.buckets, v); i < len(h.buckets) {
		s.counts[i]++
	}
	s.sum += v
	s.count++
}

func (h *Histogram) write(w io.Writer) error {
	h.mu.Lock()
	keys := make([]string, 0, len(h.vals))
	for k := range h.vals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		s := h.vals[k]
		if at, ok := h.fresh[k]; ok {
			ts := " 0 " + strconv.FormatInt(at, 10) + "\n"
			for _, ub := range h.buckets {
				b.WriteString(h.sampleName(k) + "_bucket" + h.labelSet(k, "le", num(ub)) + ts)
			}
			b.WriteString(h.sampleName(k) + "_bucket" + h.labelSet(k, "le", "+Inf") + ts)
			b.WriteString(h.sampleName(k) + "_sum" + h.labelSet(k) + ts)
			b.WriteString(h.sampleName(k) + "_count" + h.labelSet(k) + ts)
			delete(h.fresh, k)
		}
		var cum uint64
		for i, ub := range h.buckets {
			cum += s.counts[i]
			b.WriteString(h.sampleName(k) + "_bucket" + h.labelSet(k, "le", num(ub)) + " " + strconv.FormatUint(cum, 10) + "\n")
		}
		b.WriteString(h.sampleName(k) + "_bucket" + h.labelSet(k, "le", "+Inf") + " " + strconv.FormatUint(s.count, 10) + "\n")
		b.WriteString(h.sampleName(k) + "_sum" + h.labelSet(k) + " " + num(s.sum) + "\n")
		b.WriteString(h.sampleName(k) + "_count" + h.labelSet(k) + " " + strconv.FormatUint(s.count, 10) + "\n")
	}
	h.mu.Unlock()
	if b.Len() == 0 {
		return nil
	}
	if err := h.header(w); err != nil {
		return err
	}
	_, err := io.WriteString(w, b.String())
	return err
}
