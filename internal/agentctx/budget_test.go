package agentctx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func assertJSONBudget(t *testing.T, value any, used, limit int) {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	assert.Equal(t, used, (len(data)+3)/4, "estimate covers all JSON fields and escaping")
	assert.LessOrEqual(t, len(data), limit*4)
}

func budgetFixture(t *testing.T, summary string) Service {
	t.Helper()
	g := model.NewGraph()
	root := model.Node{ID: storetest.ID(model.KindFunction, "app.RareTarget"), Repo: storetest.Repo,
		Kind: model.KindFunction, Name: "RareTarget", QualifiedName: "app.RareTarget", Summary: summary,
		File: "root.go", StartLine: 1, EndLine: 10, Exported: true, Revision: "revision-a"}
	g.AddNode(root)
	for i := range 40 {
		n := model.Node{ID: storetest.ID(model.KindFunction, fmt.Sprintf("app.RareTargetCaller%02d", i)), Repo: storetest.Repo,
			Kind: model.KindFunction, Name: fmt.Sprintf("RareTargetCaller%02d", i), QualifiedName: fmt.Sprintf("app.RareTargetCaller%02d", i),
			Summary: summary, File: "callers.go", StartLine: i*10 + 1, EndLine: i*10 + 9, Exported: true}
		g.AddNode(n)
		g.AddEdge(model.Edge{From: n.ID, To: root.ID, Kind: model.EdgeCalls})
	}
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(context.Background(), storetest.Repo, g))
	return Service{R: s}
}

func TestMinimalContextCompleteJSONBudget(t *testing.T) {
	s := budgetFixture(t, strings.Repeat("\x00<&\"\\", 500))
	for _, requested := range []int{1, 256, 512, 1500, 20000, 1000000} {
		t.Run(fmt.Sprint(requested), func(t *testing.T) {
			got, err := s.MinimalContext(context.Background(), storetest.Repo, "RareTarget", nil, requested)
			require.NoError(t, err)
			assertJSONBudget(t, got, got.UsedTokens, got.Budget)
			assert.Equal(t, got.Budget, contextBudget(requested, 1500))
			assert.Equal(t, got.Estimator, JSONEstimator)
			require.True(t, len(got.Items) > 0)
			assert.Equal(t, got.Items[0].QualifiedID, "app.RareTarget")
			assert.Equal(t, got.Items[0].Retrieval, "exact")
			assert.Equal(t, got.Items[0].Revision, "revision-a")
			if requested <= 1500 {
				assert.True(t, got.Items[0].DetailOmitted)
			}
			assert.Equal(t, len(got.Items)+got.Omitted, 41)
			again, err := s.MinimalContext(context.Background(), storetest.Repo, "RareTarget", nil, requested)
			require.NoError(t, err)
			a, err := json.Marshal(got)
			require.NoError(t, err)
			b, err := json.Marshal(again)
			require.NoError(t, err)
			assert.Equal(t, string(a), string(b), "packing is deterministic")
		})
	}
}

func TestMinimalContextNoMatchAndUnfittableEnvelope(t *testing.T) {
	s := svc(t)
	got, err := s.MinimalContext(context.Background(), storetest.Repo, "no_such_symbol", nil, 256)
	require.NoError(t, err)
	assertJSONBudget(t, got, got.UsedTokens, got.Budget)
	assert.Equal(t, got.Omitted, 0)
	assert.Contains(t, got.Next, "Nothing matched")
	_, err = s.MinimalContext(context.Background(), storetest.Repo, strings.Repeat("<", 1000), nil, 256)
	assert.True(t, errors.Is(err, ErrResponseBudget), "escaped task and envelope count toward the budget")
}

func TestCompositeResponseBudgets(t *testing.T) {
	s := budgetFixture(t, strings.Repeat("<&\"", 1000))
	ctx := context.Background()
	t.Run("symbol", func(t *testing.T) {
		got, err := s.SymbolContext(ctx, storetest.Repo, "app.RareTarget", ContextOptions{Verbosity: Full, Limit: 100000, Depth: 100000, MaxTokens: 512})
		require.NoError(t, err)
		assertJSONBudget(t, got, got.Budget.UsedTokens, 512)
		assert.Equal(t, got.Symbol.QualifiedID, "app.RareTarget")
		assert.Equal(t, got.Counts.Callers, 40)
		require.NotNil(t, got.Risk)
		assert.Greater(t, got.Budget.Omitted, 0)
		assert.Contains(t, got.Budget.Next, "Evidence was omitted")
	})
	t.Run("call graph", func(t *testing.T) {
		got, err := s.CallGraph(ctx, storetest.Repo, "app.RareTarget", "both", 100000, 100000, 512)
		require.NoError(t, err)
		assertJSONBudget(t, got, got.Budget.UsedTokens, 512)
		assert.Equal(t, got.Root.QualifiedID, "app.RareTarget")
		assert.Greater(t, got.Budget.Omitted, 0)
		present := map[string]bool{got.Root.QualifiedID: true}
		for _, node := range got.Nodes {
			present[node.QualifiedID] = true
		}
		for _, edge := range got.Edges {
			assert.True(t, present[edge.From] && present[edge.To], "no dangling endpoints after truncation")
		}
	})
	t.Run("change impact", func(t *testing.T) {
		got, err := s.PRContext(ctx, storetest.Repo, PROptions{Files: []string{"root.go"}, Depth: 100000, MaxTokens: 512})
		require.NoError(t, err)
		assertJSONBudget(t, got, got.Budget.UsedTokens, 512)
		assert.Equal(t, got.Summary.ChangedSymbols, 1)
		assert.Equal(t, got.Summary.ImpactedSymbols, 40)
		assert.Greater(t, got.Budget.Omitted, 0)
	})
}

func TestCallGraphLimitPreservesBothDirections(t *testing.T) {
	s := svc(t)
	got, err := s.CallGraph(context.Background(), storetest.Repo, "app.Handler", "both", 1, 1)
	require.NoError(t, err)
	require.Len(t, got.Nodes, 2, "limit applies per direction")
	assert.Equal(t, got.Budget.Omitted, 0)
	incoming, outgoing := false, false
	for _, edge := range got.Edges {
		incoming = incoming || edge.To == "app.Handler"
		outgoing = outgoing || edge.From == "app.Handler"
	}
	assert.True(t, incoming, "caller survives")
	assert.True(t, outgoing, "callee survives")
}

func TestPublicCallerCoverageFiltersBeforeLimit(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	root := model.Node{ID: storetest.ID(model.KindFunction, "app.Target"), Repo: storetest.Repo, Kind: model.KindFunction, Name: "Target", QualifiedName: "app.Target"}
	g.AddNode(root)
	for i := range 23 {
		name := fmt.Sprintf("aPrivate%02d", i)
		public := i >= 20
		if public {
			name = fmt.Sprintf("zPublic%02d", i)
		}
		n := model.Node{ID: storetest.ID(model.KindFunction, "app."+name), Repo: storetest.Repo, Kind: model.KindFunction, Name: name, QualifiedName: "app." + name, Exported: public}
		g.AddNode(n)
		g.AddEdge(model.Edge{From: n.ID, To: root.ID, Kind: model.EdgeCalls})
	}
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, g))
	s := Service{R: st}
	got, err := s.SymbolContext(ctx, storetest.Repo, "app.Target", ContextOptions{Verbosity: Full})
	require.NoError(t, err)
	require.Len(t, got.UntestedCallers, 3, "private candidates must not consume the public coverage limit")
	assert.Equal(t, got.Counts.CallerTestsChecked, 3)
	assert.Equal(t, got.Counts.CallerTestsOmitted, 0)
	got, err = s.SymbolContext(ctx, storetest.Repo, "app.Target", ContextOptions{Verbosity: Full, Limit: 1})
	require.NoError(t, err)
	require.Len(t, got.UntestedCallers, 1)
	assert.Equal(t, got.Counts.CallerTestsChecked, 1)
	assert.Equal(t, got.Counts.CallerTestsOmitted, 2)
	assert.Contains(t, got.Hint, "1 of 3")
}

func TestMinimalContextProsePunctuation(t *testing.T) {
	for _, task := range []string{"Handler: fix nil checks", "app.Handler: fix nil checks", "Handler- fix nil checks"} {
		t.Run(task, func(t *testing.T) {
			got, err := svc(t).MinimalContext(context.Background(), storetest.Repo, task, nil, 512)
			require.NoError(t, err)
			require.True(t, len(got.Items) > 0)
			assert.Equal(t, got.Items[0].QualifiedID, "app.Handler")
			assert.Equal(t, got.Items[0].Retrieval, "exact")
		})
	}
	kw := keywords("E-42 policy:7.2")
	assert.Contains(t, kw, "E-42")
	assert.Contains(t, kw, "policy:7.2")
}

type noisySearch struct {
	store.Reader
	nodes []model.Node
}

func (s noisySearch) Search(context.Context, string, string, []model.NodeKind, int) ([]model.Node, error) {
	return s.nodes, nil
}

func TestExactQualifiedLookupSurvivesCandidateCap(t *testing.T) {
	s := svc(t)
	// A backend's capped lexical candidates need not contain the exact node.
	s.R = noisySearch{Reader: s.R}
	n, err := s.Resolve(context.Background(), storetest.Repo, "app.Handler")
	require.NoError(t, err)
	assert.Equal(t, n.ShortName(), "app.Handler")
	got, err := s.MinimalContext(context.Background(), storetest.Repo, "app.Handler", nil, 512)
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	assert.Equal(t, got.Items[0].QualifiedID, "app.Handler")
	assert.Equal(t, got.Items[0].Retrieval, "exact")
}

func TestExactMatchOutranksApproximateEvidence(t *testing.T) {
	ctx := context.Background()
	s := svc(t)
	s.R = noisySearch{Reader: s.R, nodes: []model.Node{
		{ID: storetest.ID(model.KindFunction, "app.RareTargetExtra"), Repo: storetest.Repo, Kind: model.KindFunction, Name: "RareTargetExtra", QualifiedName: "app.RareTargetExtra", Exported: true},
		{ID: storetest.ID(model.KindFunction, "app.RareTarget"), Repo: storetest.Repo, Kind: model.KindFunction, Name: "RareTarget", QualifiedName: "app.RareTarget", IsTest: true},
	}}
	got, err := s.MinimalContext(ctx, storetest.Repo, "retry cache auth RareTarget", nil, 512)
	require.NoError(t, err)
	require.Len(t, got.Items, 2)
	assert.Equal(t, got.Items[0].QualifiedID, "app.RareTarget")
	assert.Equal(t, got.Items[0].Retrieval, "exact")
	assert.Equal(t, got.Items[1].Retrieval, "lexical")
}

func TestForeignSearchHitsAndSuggestionsAreDiscarded(t *testing.T) {
	s := svc(t)
	s.R = noisySearch{Reader: s.R, nodes: []model.Node{{ID: "foreign|Function|secret", Repo: "foreign", Kind: model.KindFunction, Name: "secret", QualifiedName: "foreign.secret", Summary: "private evidence"}}}
	got, err := s.MinimalContext(context.Background(), storetest.Repo, "secret", nil, 512)
	require.NoError(t, err)
	require.Len(t, got.Items, 0)
	_, err = s.Resolve(context.Background(), storetest.Repo, "secret")
	require.Error(t, err)
	assert.False(t, strings.Contains(err.Error(), "foreign"))
	assert.False(t, strings.Contains(err.Error(), "private evidence"))
}

func TestForeignAndStaleDuplicateMetadataIsDiscarded(t *testing.T) {
	g := storetest.Fixture()
	rn, ok := g.Node(storetest.ID(model.KindRepository, storetest.Repo))
	require.True(t, ok)
	enc, err := duplicates.Encode(duplicates.Report{Groups: []duplicates.Group{{Kind: "exact", Similarity: 1, Copies: 3,
		Members: []duplicates.Member{
			{ID: storetest.ID(model.KindFunction, "app.Handler"), Name: "app.Handler"},
			{ID: "foreign|Function|secret", Name: "foreign.secret", File: "private.go"},
			{ID: storetest.ID(model.KindFunction, "app.Deleted"), Name: "app.Deleted", File: "stale.go"},
		},
	}}})
	require.NoError(t, err)
	rn.Duplicates = enc
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(context.Background(), storetest.Repo, g))
	service := Service{R: s}
	symbol, err := service.SymbolContext(context.Background(), storetest.Repo, "app.Handler", ContextOptions{})
	require.NoError(t, err)
	assert.True(t, symbol.Duplicate == nil)
	pr, err := service.PRContext(context.Background(), storetest.Repo, PROptions{Files: []string{"app/app.go"}})
	require.NoError(t, err)
	assert.Equal(t, pr.Summary.DuplicatedChanges, 0)
	data, err := json.Marshal(pr)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(data), "foreign"))
	assert.False(t, strings.Contains(string(data), "private.go"))
	assert.False(t, strings.Contains(string(data), "stale.go"))
}

type deadlineReader struct {
	store.Reader
	deadline time.Time
}

func (r *deadlineReader) Search(ctx context.Context, repo, q string, kinds []model.NodeKind, limit int) ([]model.Node, error) {
	r.deadline, _ = ctx.Deadline()
	return r.Reader.Search(ctx, repo, q, kinds, limit)
}

func TestContextInputBoundsAndCancellation(t *testing.T) {
	s := svc(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.MinimalContext(ctx, storetest.Repo, "Handler", nil, 1500)
	assert.True(t, errors.Is(err, context.Canceled))
	_, err = s.MinimalContext(context.Background(), storetest.Repo, strings.Repeat("x", 8193), nil, 1500)
	require.Error(t, err)
	_, err = s.PRContext(context.Background(), storetest.Repo, PROptions{Files: make([]string, 101)})
	require.Error(t, err)
	_, err = s.PRContext(context.Background(), storetest.Repo, PROptions{Diff: make([]byte, 4<<20+1)})
	require.Error(t, err)
	r := &deadlineReader{Reader: s.R}
	s.R = r
	start := time.Now()
	_, err = s.MinimalContext(context.Background(), storetest.Repo, "Handler", nil, 1500)
	require.NoError(t, err)
	assert.False(t, r.deadline.IsZero())
	assert.True(t, r.deadline.Sub(start) <= 11*time.Second)
}

func FuzzMinimalContextBudget(f *testing.F) {
	f.Add("Handler", 256)
	f.Add("<>&\"\\\x00", 512)
	f.Add("Handler\u2028Handler", 1500)
	f.Fuzz(func(t *testing.T, task string, requested int) {
		if len(task) > 8192 {
			return
		}
		got, err := svc(t).MinimalContext(context.Background(), storetest.Repo, task, nil, requested)
		if err != nil {
			assert.True(t, errors.Is(err, ErrResponseBudget))
			return
		}
		assertJSONBudget(t, got, got.UsedTokens, got.Budget)
	})
}
