package security

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type countedReader struct {
	store.Reader
	calls int
}

func (r *countedReader) Node(ctx context.Context, id string) (model.Node, error) {
	r.calls++
	return r.Reader.Node(ctx, id)
}

func (r *countedReader) List(ctx context.Context, repo string, f store.Filter, m store.Metric, limit int) ([]store.Ranked, error) {
	r.calls++
	return r.Reader.List(ctx, repo, f, m, limit)
}

func (r *countedReader) Traverse(ctx context.Context, id string, kinds []model.EdgeKind, dir store.Direction, depth, limit int) ([]model.Hit, error) {
	r.calls++
	return r.Reader.Traverse(ctx, id, kinds, dir, depth, limit)
}

func TestSummaryPreservesSecurityAggregates(t *testing.T) {
	for _, suppressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "suppressed"}[suppressed], func(t *testing.T) {
			r := &countedReader{Reader: fixture(t)}
			opts := Options{}
			if suppressed {
				opts.Suppression = func(n model.Node) string {
					if n.Rule == "exec-shell" {
						return "approved"
					}
					return ""
				}
			}
			full, err := Build(context.Background(), r, storetest.Repo, opts)
			require.NoError(t, err)
			fullCalls := r.calls
			r.calls = 0
			opts.SummaryOnly = true
			summary, err := Build(context.Background(), r, storetest.Repo, opts)
			require.NoError(t, err)
			assert.Equal(t, summary.Score, full.Score)
			assert.Equal(t, summary.Grade, full.Grade)
			assert.Equal(t, summary.FindingCounts, full.FindingCounts)
			assert.Equal(t, summary.SuppressedCounts, full.SuppressedCounts)
			assert.Equal(t, summary.VulnCounts, full.VulnCounts)
			assert.Equal(t, summary.Modules, full.Modules)
			assert.Equal(t, summary.Coverage, full.Coverage)
			assert.Equal(t, summary.Rules, full.Rules)
			assert.Len(t, summary.Sensitive, 0)
			for _, v := range summary.Vulnerabilities {
				assert.Len(t, v.CallSites, 0)
			}
			assert.Less(t, r.calls, fullCalls)
			t.Logf("graph Node/List/Traverse calls: full=%d summary=%d", fullCalls, r.calls)
		})
	}
}
