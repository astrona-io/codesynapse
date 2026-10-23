package agentctx

import (
	"fmt"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
)

func BenchmarkAuditMinimalContext(b *testing.B) {
	for _, tc := range []struct {
		name    string
		nodes   int
		summary string
	}{
		{"small", 100, "A synthetic context selection candidate."},
		{"large", 10000, "A synthetic context selection candidate."},
		{"escaped", 10000, strings.Repeat("\x00<&\"\\", 100)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			const repo = "audit-context"
			g := model.NewGraph()
			for i := range tc.nodes {
				name := fmt.Sprintf("RareTarget%05d", i)
				if i == 0 {
					name = "RareTarget"
				}
				g.AddNode(model.Node{
					ID: model.NodeID(repo, model.KindFunction, "audit."+name), Repo: repo,
					Kind: model.KindFunction, Name: name, QualifiedName: "audit." + name,
					File: "audit.go", StartLine: i*3 + 1, EndLine: i*3 + 2, Summary: tc.summary,
				})
			}
			st := memory.New()
			if err := st.ReplaceRepo(b.Context(), repo, g); err != nil {
				b.Fatal(err)
			}
			svc := Service{R: st}
			check, err := svc.MinimalContext(b.Context(), repo, "audit.RareTarget", nil, 1500)
			if err != nil || len(check.Items) == 0 || check.Items[0].QualifiedID != "audit.RareTarget" {
				b.Fatalf("exact evidence lost: %v; %v", check.Items, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				out, err := svc.MinimalContext(b.Context(), repo, "audit.RareTarget", nil, 1500)
				if err != nil || out.UsedTokens > 1500 {
					b.Fatalf("context budget failed: %d; %v", out.UsedTokens, err)
				}
			}
		})
	}
}
