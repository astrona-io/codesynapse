package memory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// searchReference is the Search implementation before phase 4j.3: fold and
// collect every match, then sort them all. It stays as the oracle for the
// equivalence tests and the baseline for the benchmarks.
func searchReference(rg *repoGraph, q string, kinds []model.NodeKind, limit int) []model.Node {
	limit = store.ClampLimit(limit, 20)
	lq := strings.ToLower(q)
	var out []model.Node
	for _, n := range rg.nodes {
		if len(kinds) > 0 && !slices.Contains(kinds, n.Kind) {
			continue
		}
		if model.ContainsFold(n.Name, q) || model.ContainsFold(n.QualifiedName, q) {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b model.Node) int {
		ea, eb := strings.ToLower(a.Name) == lq, strings.ToLower(b.Name) == lq
		if ea != eb {
			if ea {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(len(a.QualifiedName), len(b.QualifiedName)), cmp.Compare(a.ID, b.ID))
	})
	return out[:min(len(out), limit)]
}

var searchKinds = []model.NodeKind{model.KindFunction, model.KindMethod, model.KindType}

// searchFixture builds n nodes named RareTarget00000…, with RareTarget itself
// first and the kinds rotating, so prefix queries match nearly everything and
// kind filters drop a third.
func searchFixture(t testing.TB, repo string, n int) *Store {
	t.Helper()
	g := model.NewGraph()
	for i := range n {
		name := fmt.Sprintf("RareTarget%05d", i)
		if i == 0 {
			name = "RareTarget"
		}
		kind := searchKinds[i%len(searchKinds)]
		g.AddNode(model.Node{
			ID: model.NodeID(repo, kind, "audit."+name), Repo: repo, Kind: kind,
			Name: name, QualifiedName: "audit." + name, File: "audit.go", StartLine: i + 1, EndLine: i + 1,
		})
	}
	st := New()
	require.NoError(t, st.ReplaceRepo(context.Background(), repo, g))
	return st
}

func nodeIDs(nodes []model.Node) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	return ids
}

func assertMatchesReference(t *testing.T, st *Store, repo, q string, kinds []model.NodeKind, limit int) {
	t.Helper()
	got, err := st.Search(context.Background(), repo, q, kinds, limit)
	require.NoError(t, err)
	assert.Equal(t, nodeIDs(got), nodeIDs(searchReference(st.repos[repo], q, kinds, limit)))
}

func TestSearchMatchesReference(t *testing.T) {
	const repo = "search"
	g := model.NewGraph()
	for i, name := range []string{
		"Handler", "handler", "HANDLER", "NewHandler", "handlerFunc", "Serve",
		"İstanbul", "istanbul", "ẞtraße", "Straße", "K", "k", "", "x",
	} {
		kind := searchKinds[i%len(searchKinds)]
		qualified := "pkg." + name
		if i%2 == 0 {
			qualified = "(*pkg.T)." + name
		}
		g.AddNode(model.Node{ID: model.NodeID(repo, kind, fmt.Sprintf("%s#%d", qualified, i)), Repo: repo, Kind: kind, Name: name, QualifiedName: qualified})
	}
	st := New()
	require.NoError(t, st.ReplaceRepo(context.Background(), repo, g))
	big := searchFixture(t, "big", 3000)
	for _, tt := range []struct {
		name  string
		st    *Store
		repo  string
		q     string
		kinds []model.NodeKind
		limit int
	}{
		{"exact before longer names", st, repo, "handler", nil, 10},
		{"exact limited", st, repo, "handler", nil, 2},
		{"exact filtered by kind", st, repo, "HANDLER", []model.NodeKind{model.KindMethod}, 10},
		{"qualified name only", st, repo, "(*pkg", nil, 50},
		{"unicode folding", st, repo, "İ", nil, 10},
		{"unicode length change", st, repo, "straße", nil, 10},
		{"empty query", st, repo, "", nil, 50},
		{"default limit", st, repo, "", nil, 0},
		{"no match", st, repo, "nothing", nil, 10},
		{"common prefix", big, "big", "raretarget0", nil, 20},
		{"common prefix past limit", big, "big", "RareTarget", nil, store.MaxLimit},
		{"mixed kinds", big, "big", "target01", []model.NodeKind{model.KindType, model.KindFunction}, 30},
		{"no match large", big, "big", "zzz", nil, 20},
	} {
		t.Run(tt.name, func(t *testing.T) { assertMatchesReference(t, tt.st, tt.repo, tt.q, tt.kinds, tt.limit) })
	}
}

func FuzzSearchMatchesReference(f *testing.F) {
	f.Add("Handler,handler,NewHandler,İd,ID", "handler", uint8(0), uint8(3))
	f.Add("a,A,ab,ẞ,ss", "a", uint8(5), uint8(1))
	f.Add(",,x", "", uint8(2), uint8(0))
	f.Fuzz(func(t *testing.T, names, q string, kindMask, limit uint8) {
		const repo = "fuzz"
		parts := strings.Split(names, ",")
		if len(parts) > 64 {
			parts = parts[:64]
		}
		g := model.NewGraph()
		for i, name := range parts {
			kind := searchKinds[i%len(searchKinds)]
			g.AddNode(model.Node{ID: model.NodeID(repo, kind, fmt.Sprintf("%d", i)), Repo: repo, Kind: kind, Name: name, QualifiedName: "p." + name})
		}
		st := New()
		require.NoError(t, st.ReplaceRepo(context.Background(), repo, g))
		var kinds []model.NodeKind
		for i, k := range searchKinds {
			if kindMask&(1<<i) != 0 {
				kinds = append(kinds, k)
			}
		}
		assertMatchesReference(t, st, repo, q, kinds, int(limit))
	})
}

func TestSearchRepositoryFence(t *testing.T) {
	st := searchFixture(t, "one", 10)
	other := searchFixture(t, "two", 10)
	st.repos["two"] = other.repos["two"]
	got, err := st.Search(context.Background(), "one", "RareTarget", nil, 50)
	require.NoError(t, err)
	require.Len(t, got, 10)
	for _, n := range got {
		assert.Equal(t, n.Repo, "one")
	}
	_, err = st.Search(context.Background(), "three", "RareTarget", nil, 50)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}

func TestSearchCancelled(t *testing.T) {
	st := searchFixture(t, "cancel", 3*searchCancelEvery)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := st.Search(ctx, "cancel", "zzz", nil, 10)
	assert.True(t, errors.Is(err, context.Canceled))

	// Cancelled during the scan: the first check passes, a later one sees it.
	mid := &cancelAfter{Context: context.Background(), checks: 2}
	_, err = st.Search(mid, "cancel", "zzz", nil, 10)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Equal(t, mid.calls, 3)
}

// cancelAfter reports cancellation once Err has been called more than checks
// times.
type cancelAfter struct {
	context.Context
	checks, calls int
}

func (c *cancelAfter) Err() error {
	c.calls++
	if c.calls > c.checks {
		return context.Canceled
	}
	return nil
}

// searchCases are the workloads the 4j.3 numbers in
// docs/CODE_ARCHITECTURE_AUDIT.md come from.
var searchCases = []struct {
	name  string
	q     string
	kinds []model.NodeKind
}{
	{"exact", "RareTarget", nil},
	{"common_prefix", "raretarget0", nil},
	{"no_match", "zzz", nil},
	{"mixed_kinds", "target01", []model.NodeKind{model.KindType, model.KindFunction}},
}

func BenchmarkSearch(b *testing.B) {
	for _, size := range []int{100, 10000} {
		st := searchFixture(b, "bench", size)
		rg := st.repos["bench"]
		for _, tc := range searchCases {
			b.Run(fmt.Sprintf("nodes_%d/%s/indexed", size, tc.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := st.Search(b.Context(), "bench", tc.q, tc.kinds, 20); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(fmt.Sprintf("nodes_%d/%s/reference", size, tc.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					st.mu.RLock()
					searchReference(rg, tc.q, tc.kinds, 20)
					st.mu.RUnlock()
				}
			})
		}
	}
}

// BenchmarkSearchParallel measures searches contending for the store's read
// lock while one goroutine per processor queries.
func BenchmarkSearchParallel(b *testing.B) {
	st := searchFixture(b, "bench", 10000)
	for _, tc := range searchCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := st.Search(context.Background(), "bench", tc.q, tc.kinds, 20); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

func BenchmarkReplaceRepoSearchIndex(b *testing.B) {
	g := model.NewGraph()
	for i := range 10000 {
		name := fmt.Sprintf("RareTarget%05d", i)
		g.AddNode(model.Node{ID: model.NodeID("bench", model.KindFunction, name), Repo: "bench", Kind: model.KindFunction, Name: name, QualifiedName: "audit." + name})
	}
	st := New()
	b.ReportAllocs()
	for b.Loop() {
		if err := st.ReplaceRepo(b.Context(), "bench", g); err != nil {
			b.Fatal(err)
		}
	}
}
