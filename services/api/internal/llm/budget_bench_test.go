package llm

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/execution"
)

func BenchmarkPromptPreflight(b *testing.B) {
	ctx, err := execution.With(b.Context(), execution.Identity{TenantID: "benchmark", SubjectID: "subject", SessionID: "thread", ParentSessionID: "thread"})
	if err != nil {
		b.Fatal(err)
	}
	cfg := Config{Providers: []ProviderConfig{{Name: "benchmark", Kind: KindOpenAI, Model: "unverified", MaxInputTokens: 16 << 20}}, Default: "benchmark"}
	r := NewRegistry(cfg)
	for _, tc := range []struct{ name, text string }{
		{"small-code", strings.Repeat("func Compute() int { return 1 }\n", 16)},
		{"code-32KiB", strings.Repeat("func Compute() int { return 1 }\n", 1024)},
		{"unicode", strings.Repeat("日本語 λ → café\n", 2048)},
		{"escaping", strings.Repeat("\"\\\t\n<>&", 4096)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			req := Request{System: "Return a bounded answer.", User: tc.text}
			b.ReportAllocs()
			b.SetBytes(int64(len(req.System) + len(req.User)))
			for b.Loop() {
				if _, err := r.Preflight(ctx, "benchmark", req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("registry-startup", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			fresh := NewRegistry(cfg)
			if _, err := fresh.Preflight(ctx, "benchmark", Request{User: "hello"}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("parallel-code", func(b *testing.B) {
		req := Request{System: "Return a bounded answer.", User: strings.Repeat("func Compute() int { return 1 }\n", 1024)}
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, err := r.Preflight(ctx, "benchmark", req); err != nil {
					b.Error(err)
					return
				}
			}
		})
	})
}
