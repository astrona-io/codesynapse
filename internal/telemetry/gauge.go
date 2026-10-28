package telemetry

import (
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"sync"
)

// Gauge holds the latest value of a bounded set of observations.
type Gauge struct {
	family
	mu   sync.Mutex
	vals map[string]float64
}

// UpdateGauges publishes related gauge changes as one export snapshot.
// The callback must not export metrics or call UpdateGauges recursively.
func (r *Registry) UpdateGauges(update func()) {
	r.gaugesMu.Lock()
	defer r.gaugesMu.Unlock()
	update()
}

// ScopedGauge registers a current-value metric with trusted core attributes.
func (r *Registry) ScopedGauge(name, help string, labels ...string) *Gauge {
	g := &Gauge{family: newFamily(r, name, help, "gauge", labels), vals: map[string]float64{}}
	g.scope()
	r.add(g)
	return g
}

// Gauge registers a current-value metric without core attributes, for
// process state that belongs to no tenant. Its name must start with
// platform_, so it never mixes with tenant series.
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	if !strings.HasPrefix(name, "platform_") {
		panic("telemetry: unscoped gauge " + name + " must be named platform_*") // programmer error
	}
	g := &Gauge{family: newFamily(r, name, help, "gauge", labels), vals: map[string]float64{}}
	r.add(g)
	return g
}

// SetContext replaces a current observation; non-finite values are ignored.
func (g *Gauge) SetContext(ctx context.Context, v float64, values ...string) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	key, ok := g.contextKey(ctx, values)
	if !ok {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.vals[key]; !exists && len(g.vals) >= g.budget {
		g.reg.dropped.IncContext(ctx, g.name)
		return
	}
	g.vals[key] = v
}

// DeleteContext releases a series that is no longer observed.
func (g *Gauge) DeleteContext(ctx context.Context, values ...string) {
	key, ok := g.contextKey(ctx, values)
	if !ok {
		return
	}
	g.mu.Lock()
	delete(g.vals, key)
	g.mu.Unlock()
}

func (g *Gauge) write(w io.Writer) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	keys := make([]string, 0, len(g.vals))
	for k := range g.vals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if len(keys) == 0 {
		return nil
	}
	if err := g.header(w); err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := fmt.Fprintf(w, "%s%s %s\n", g.sampleName(k), g.labelSet(k), num(g.vals[k])); err != nil {
			return err
		}
	}
	return nil
}
