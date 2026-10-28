package agentctx

import (
	"context"
	"slices"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// rankFixture holds the symbols two real handoff_context briefs (PLAN item
// 18, 2026-10-28) put under "Start here" by matching task words, beside the
// ones the tasks named.
func rankFixture(t *testing.T) Service {
	t.Helper()
	g := model.NewGraph()
	line := 1
	add := func(kind model.NodeKind, name, qualified, file string) {
		g.AddNode(model.Node{ID: storetest.ID(kind, qualified), Repo: storetest.Repo, Kind: kind, Name: name,
			QualifiedName: qualified, File: file, StartLine: line, EndLine: line + 5, Exported: name[0] >= 'A' && name[0] <= 'Z'})
		line += 10
	}
	// What the tasks name.
	add(model.KindFunction, "requirePlatform", "accounts.requirePlatform", "internal/accounts/admin.go")
	add(model.KindType, "Service", "accounts.Service", "internal/accounts/service.go")
	add(model.KindFunction, "agentStats", "api.agentStats", "services/api/internal/api/usage.go")
	add(model.KindType, "PriceReference", "llm.PriceReference", "services/api/internal/llm/pricing.go")
	// What the generic words found.
	add(model.KindFunction, "ImplementationsIn", "coretools.ImplementationsIn", "internal/coretools/coretools.go")
	add(model.KindFunction, "Implementations", "coretools.Implementations", "internal/coretools/coretools.go")
	add(model.KindMethod, "Implementations", "(agentctx.Service).Implementations", "internal/agentctx/graph.go")
	add(model.KindMethod, "only", "(accounts.AuditLog).only", "internal/accounts/audit.go")
	add(model.KindFunction, "RecordIngestPhase", "telemetry.RecordIngestPhase", "internal/telemetry/metrics.go")
	add(model.KindMethod, "token", "(*ssotest.Provider).token", "internal/sso/ssotest/provider.go")
	add(model.KindMethod, "hook", "(*codesynapse.gitHookOptions).hook", "cmd/codesynapse/githook.go")
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(context.Background(), storetest.Repo, g))
	return Service{R: s}
}

func TestMinimalContextNamedIdentifiersOutrankTaskWords(t *testing.T) {
	cases := []struct {
		name, task string
		want       []string
		never      []string
		below      []string
	}{{
		name: "platform stats phase 1",
		task: "Implement phase 1 of docs/PLATFORM_STATS_PLAN.md: accounts.Service.PlatformStats helper (reuses requirePlatform, only platform administrators) and a Statistics tab under Administration",
		want: []string{"accounts.requirePlatform"},
		never: []string{"coretools.ImplementationsIn", "coretools.Implementations", "(agentctx.Service).Implementations",
			"(accounts.AuditLog).only", "telemetry.RecordIngestPhase"},
	}, {
		name:  "platform stats phase 2",
		task:  "Implement phase 2 of docs/PLATFORM_STATS_PLAN.md: the hook counter tokens:cache-known-turns, agentStats and PriceReference for the token savings share",
		want:  []string{"api.agentStats", "llm.PriceReference"},
		never: []string{"coretools.ImplementationsIn", "coretools.Implementations", "telemetry.RecordIngestPhase"},
		below: []string{"(*ssotest.Provider).token", "(*codesynapse.gitHookOptions).hook"},
	}}
	s := rankFixture(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.MinimalContext(context.Background(), storetest.Repo, c.task, nil, 4000)
			require.NoError(t, err)
			ids := make([]string, len(got.Items))
			for i, it := range got.Items {
				ids[i] = it.QualifiedID
			}
			require.True(t, len(ids) >= len(c.want), ids)
			for i, w := range c.want {
				assert.True(t, slices.Contains(ids[:len(c.want)], w), "named identifier first", w, ids)
				assert.Equal(t, got.Items[i].Retrieval, "exact")
			}
			for _, n := range c.never {
				assert.False(t, slices.Contains(ids, n), "matched only a generic task word", n, ids)
			}
			for _, b := range c.below {
				if i := slices.Index(ids, b); i >= 0 {
					assert.True(t, i >= len(c.want), "a prose word outranks a named identifier", b, ids)
					assert.Equal(t, got.Items[i].Retrieval, "lexical")
				}
			}
		})
	}
}

func TestKeywordsPutNamedIdentifiersFirst(t *testing.T) {
	kw := keywords("Implement phase 2 of the hook counter for the token savings share, then wire agentStats and PriceReference")
	assert.Equal(t, kw[0], "agentStats")
	assert.Equal(t, kw[1], "PriceReference")
	for _, generic := range []string{"Implement", "phase", "only", "start"} {
		assert.NotContains(t, kw, generic)
	}
	assert.Contains(t, keywords("Handler Do main Usage"), "Handler", "a task of plain names keeps them")
}
