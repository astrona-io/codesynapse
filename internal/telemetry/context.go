package telemetry

import (
	"context"
	"slices"
	"strings"
	"sync"
)

type legacyKey struct{}

var legacyContext = context.WithValue(context.Background(), legacyKey{}, true)

type toolKey struct{}
type modelKey struct{}
type modelIdentity struct{ provider, model string }

var models = struct {
	sync.RWMutex
	allowed map[modelIdentity]bool
}{allowed: map[modelIdentity]bool{}}

// AllowModels registers identities from server configuration, never provider responses.
func AllowModels(provider string, names ...string) {
	models.Lock()
	defer models.Unlock()
	for _, name := range names {
		if name == "" {
			name = "none"
		}
		if provider != "" {
			models.allowed[modelIdentity{provider, name}] = true
		}
	}
}

func configuredModel(provider, model string) modelIdentity {
	if model == "" {
		model = "none"
	}
	models.RLock()
	defer models.RUnlock()
	if models.allowed[modelIdentity{provider, model}] {
		return modelIdentity{provider, model}
	}
	return modelIdentity{"none", "none"}
}

// ModelAllowed reports whether provider and model were registered with
// AllowModels in this process. Readers of stored series use it to name only
// configured models; anything else is shown folded.
func ModelAllowed(provider, model string) bool {
	if provider == "" || provider == "none" || model == "" || model == "none" {
		return false
	}
	models.RLock()
	defer models.RUnlock()
	return models.allowed[modelIdentity{provider, model}]
}

// WithModel carries the configured provider and model used by this operation.
func WithModel(ctx context.Context, provider, model string) context.Context {
	return context.WithValue(ctx, modelKey{}, configuredModel(provider, model))
}

// WithTool carries a registered tool or a fixed transport category.
func WithTool(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, toolKey{}, acceptedTool(name))
}

func acceptedTool(name string) string {
	switch name {
	case "", "none":
		return "none"
	case "transport":
		return "transport"
	}
	return ToolLabel(name)
}

// Tool returns the accepted tool name, or none for other application work.
func Tool(ctx context.Context) string {
	if v, ok := ctx.Value(toolKey{}).(string); ok {
		return v
	}
	return "none"
}

var coreLabels = []string{"tenant_id", "mcp_tool", "llm_provider", "llm_model"}

func (f *family) scope() {
	f.scoped = true
	f.labels = slices.Clone(f.labels)
	for _, name := range coreLabels {
		if !slices.Contains(f.labels, name) {
			f.labels = append(f.labels, name)
		}
	}
}

// ScopedCounter retains its positional arguments while adding trusted core attributes.
func (r *Registry) ScopedCounter(name, help string, labels ...string) *Counter {
	c := r.Counter(name, help, labels...)
	c.scope()
	return c
}

// ScopedHistogram budgets the exported bucket, sum and count series together.
func (r *Registry) ScopedHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	h := r.Histogram(name, help, buckets, labels...)
	h.scope()
	h.budget = max(1, maxSeries/(len(h.buckets)+3))
	return h
}

func (f *family) contextKey(ctx context.Context, values []string) (string, bool) {
	if !f.scoped {
		return f.key(slices.Clone(values))
	}
	if len(values) != len(f.inputLabels) {
		return "", false
	}
	out := make([]string, len(f.labels))
	for i := range out {
		out[i] = "none"
	}
	copy(out, values)
	tenant, tool, provider, model := slices.Index(f.labels, "tenant_id"), slices.Index(f.labels, "mcp_tool"), slices.Index(f.labels, "llm_provider"), slices.Index(f.labels, "llm_model")
	if ctx.Value(legacyKey{}) != true {
		out[tenant], out[tool] = Tenant(ctx), Tool(ctx)
		m, _ := ctx.Value(modelKey{}).(modelIdentity)
		out[provider], out[model] = m.provider, m.model
	}
	if !tenantRe.MatchString(out[tenant]) {
		out[tenant] = NoTenant
	}
	out[tool] = acceptedTool(out[tool])
	m := configuredModel(out[provider], out[model])
	out[provider], out[model] = m.provider, m.model
	return f.key(out)
}

// Unauthenticated and startup work is a separate platform stream without a tenant.
func (f *family) sampleName(key string) string {
	if f.scoped && !tenantRe.MatchString(strings.Split(key, "\xff")[slices.Index(f.labels, "tenant_id")]) {
		return "platform_" + f.name
	}
	return f.name
}
