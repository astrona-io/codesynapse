package metrics_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/agentctx"
	mem "github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

type retrievalFixture struct {
	Version string            `json:"version"`
	Policy  string            `json:"policy"`
	Repo    string            `json:"repo"`
	Files   map[string]string `json:"files"`
	Nodes   []model.Node      `json:"nodes"`
	Calls   [][2]string       `json:"calls"`
	Tasks   []retrievalTask   `json:"tasks"`
}

type retrievalTask struct {
	Name        string   `json:"name"`
	Tool        string   `json:"tool"`
	Query       string   `json:"query"`
	Files       []string `json:"files"`
	Evidence    []string `json:"evidence"`
	ExpectError bool     `json:"expect_error"`
}

type latencyReport struct {
	Samples int     `json:"samples"`
	P50     float64 `json:"p50_ms"`
	P95     float64 `json:"p95_ms"`
	P99     float64 `json:"p99_ms"`
}

type retrievalResult struct {
	Task           string        `json:"task"`
	RawBytes       int           `json:"raw_bytes"`
	ResponseBytes  int           `json:"response_bytes"`
	RawTokens      int           `json:"raw_tokens"`
	ResponseTokens int           `json:"response_tokens"`
	Required       int           `json:"required_evidence"`
	Found          int           `json:"found_evidence"`
	Complete       bool          `json:"complete"`
	Missing        []string      `json:"missing_evidence,omitempty"`
	Reduction      *float64      `json:"reduction_percent"`
	ToolCalls      int           `json:"tool_calls"`
	ColdErrors     int           `json:"cold_errors"`
	WarmErrors     int           `json:"warm_errors"`
	ExpectedError  bool          `json:"expected_error"`
	Cold           latencyReport `json:"cold_fresh_store"`
	Warm           latencyReport `json:"warm_same_store"`
	Serialization  latencyReport `json:"serialization"`
	Total          latencyReport `json:"warm_total"`
}

type retrievalReport struct {
	Version        string                `json:"fixture_version"`
	RunLabel       string                `json:"run_label"`
	FixtureHash    string                `json:"fixture_sha256"`
	PolicyHash     string                `json:"policy_sha256"`
	Generated      string                `json:"generated_at"`
	Go             string                `json:"go"`
	OS             string                `json:"os"`
	Arch           string                `json:"arch"`
	CPU            string                `json:"cpu_description"`
	CPUs           int                   `json:"logical_cpus"`
	Procs          int                   `json:"gomaxprocs"`
	Concurrency    int                   `json:"concurrency"`
	SourceFiles    int                   `json:"source_files"`
	SourceBytes    int                   `json:"source_bytes"`
	GraphNodes     int                   `json:"graph_nodes"`
	Estimator      string                `json:"estimator"`
	Envelope       string                `json:"envelope"`
	Scope          string                `json:"scope"`
	RawTokens      int                   `json:"complete_raw_tokens"`
	ResponseTokens int                   `json:"complete_response_tokens"`
	Reduction      *float64              `json:"aggregate_reduction_percent"`
	Tasks          []retrievalResult     `json:"tasks"`
	Memory         memoryRetrievalReport `json:"memory"`
}

func loadRetrievalFixture(t *testing.T) (retrievalFixture, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/retrieval_v1.json")
	require.NoError(t, err)
	var f retrievalFixture
	require.NoError(t, json.Unmarshal(raw, &f))
	return f, raw
}

func fixtureService(t *testing.T, f retrievalFixture) agentctx.Service {
	t.Helper()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(f.Repo, model.KindRepository, f.Repo), Repo: f.Repo, Kind: model.KindRepository, Name: f.Repo, Head: "fixture-v1"})
	for name, text := range f.Files {
		g.AddNode(model.Node{ID: model.NodeID(f.Repo, model.KindFile, name), Repo: f.Repo, Kind: model.KindFile, Name: name, File: name, Bytes: len(text)})
	}
	for _, n := range f.Nodes {
		n.Repo = f.Repo
		n.ID = model.NodeID(f.Repo, n.Kind, n.QualifiedName)
		g.AddNode(n)
		g.AddEdge(model.Edge{From: model.NodeID(f.Repo, model.KindFile, n.File), To: n.ID, Kind: model.EdgeDefines})
	}
	for _, c := range f.Calls {
		g.AddEdge(model.Edge{From: model.NodeID(f.Repo, model.KindFunction, c[0]), To: model.NodeID(f.Repo, model.KindFunction, c[1]), Kind: model.EdgeCalls})
	}
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(t.Context(), f.Repo, g))
	foreign := model.NewGraph()
	foreign.AddNode(model.Node{ID: model.NodeID("foreign", model.KindFunction, "payments.Charge"), Repo: "foreign", Kind: model.KindFunction, Name: "Charge", QualifiedName: "payments.Charge", Summary: "FOREIGN_PRIVATE_EVIDENCE"})
	require.NoError(t, s.ReplaceRepo(t.Context(), "foreign", foreign))
	return agentctx.Service{R: s}
}

func executeRetrieval(ctx context.Context, s agentctx.Service, repo string, task retrievalTask) (any, error) {
	switch task.Tool {
	case "symbol":
		q := task.Query
		if q == "foreign-id" {
			q = model.NodeID("foreign", model.KindFunction, "payments.Charge")
		}
		return s.SymbolContext(ctx, repo, q, agentctx.ContextOptions{Verbosity: agentctx.Full, Limit: 20, Depth: 2})
	case "lookup":
		hits, err := s.R.Search(ctx, repo, task.Query, nil, 20)
		if err != nil {
			return nil, err
		}
		for _, n := range hits {
			if n.Name == task.Query {
				return s.SymbolContext(ctx, repo, n.ID, agentctx.ContextOptions{})
			}
		}
		return nil, store.ErrNotFound
	case "calls":
		return s.CallGraph(ctx, repo, task.Query, "both", 2, 20)
	case "impact":
		return s.PRContext(ctx, repo, agentctx.PROptions{Files: []string{task.Query}, Depth: 2})
	case "minimal":
		return s.MinimalContext(ctx, repo, task.Query, nil, 1500)
	}
	return nil, fmt.Errorf("unsupported fixture tool %q", task.Tool)
}

// The pinned SDK repeats successful structured output as text content. Both
// baselines include both copies; transport framing and one-time inputs do not.
func contextEnvelope(t *testing.T, value any, callErr error) []byte {
	t.Helper()
	result := &mcp.CallToolResult{}
	if callErr != nil {
		result.SetError(callErr)
	} else {
		text, err := json.Marshal(value)
		require.NoError(t, err)
		result.StructuredContent = json.RawMessage(text)
		result.Content = []mcp.Content{&mcp.TextContent{Text: string(text)}}
	}
	resultMap := map[string]any{
		"content":    result.Content,
		"_meta":      map[string]any{mcp.MetaKeyServerInfo: &mcp.Implementation{Name: "codesynapse", Version: "fixture-v1"}},
		"resultType": "complete",
	}
	if result.IsError {
		resultMap["isError"] = true
	} else {
		resultMap["structuredContent"] = result.StructuredContent
	}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": resultMap})
	require.NoError(t, err)
	return raw
}

func TestRetrievalEnvelopeMatchesSDK(t *testing.T) {
	type output struct {
		Value string `json:"value"`
	}
	value := output{Value: "punctuation: \" and 支払い"}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var toolErr error
			if fail {
				toolErr = errors.New("fixture missing")
			}
			srv := mcp.NewServer(&mcp.Implementation{Name: "codesynapse", Version: "fixture-v1"}, nil)
			mcp.AddTool(srv, &mcp.Tool{Name: "probe", Description: "Return fixture output"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, output, error) {
				return nil, value, toolErr
			})
			ct, st := mcp.NewInMemoryTransports()
			_, err := srv.Connect(t.Context(), st, nil)
			require.NoError(t, err)
			client, err := mcp.NewClient(&mcp.Implementation{Name: "retrieval-client", Version: "1"}, nil).Connect(t.Context(), ct, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			got, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "probe"})
			require.NoError(t, err)
			actual, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": got})
			require.NoError(t, err)
			var actualJSON, expectedJSON any
			require.NoError(t, json.Unmarshal(actual, &actualJSON))
			require.NoError(t, json.Unmarshal(contextEnvelope(t, value, toolErr), &expectedJSON))
			assert.Equal(t, actualJSON, expectedJSON)
			assert.Equal(t, len(contextEnvelope(t, value, toolErr)), len(actual))
		})
	}
}

func evidenceValues(t *testing.T, result any) map[string]bool {
	t.Helper()
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	var decoded any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	values := map[string]bool{}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case string:
			values[x] = true
		case []any:
			for _, e := range x {
				visit(e)
			}
		case map[string]any:
			for _, e := range x {
				visit(e)
			}
		}
	}
	visit(decoded)
	return values
}

func latency(samples []time.Duration) latencyReport {
	values := slices.Clone(samples)
	slices.Sort(values)
	p := func(percent float64) float64 {
		if len(values) == 0 {
			return 0
		}
		return float64(values[max(0, int(math.Ceil(float64(len(values))*percent))-1)]) / float64(time.Millisecond)
	}
	return latencyReport{Samples: len(values), P50: p(.5), P95: p(.95), P99: p(.99)}
}

func reductionPercent(raw, result int, complete bool) *float64 {
	if raw <= 0 || !complete {
		return nil
	}
	p := math.Round(10000*(1-float64(result)/float64(raw))) / 100
	return &p
}

func evaluateRetrieval(t *testing.T, f retrievalFixture, task retrievalTask, samples int) retrievalResult {
	t.Helper()
	s := fixtureService(t, f)
	r := retrievalResult{Task: task.Name, Required: len(task.Evidence), ToolCalls: 1, ExpectedError: task.ExpectError}
	if task.Tool == "lookup" {
		r.ToolCalls = 2
	}
	var cold, warm, serialization, total []time.Duration
	for i := range samples {
		fresh := fixtureService(t, f)
		start := time.Now()
		_, coldErr := executeRetrieval(t.Context(), fresh, f.Repo, task)
		cold = append(cold, time.Since(start))
		if coldErr != nil {
			r.ColdErrors++
		}
		start = time.Now()
		result, err := executeRetrieval(t.Context(), s, f.Repo, task)
		warm = append(warm, time.Since(start))
		if err != nil {
			r.WarmErrors++
		}
		totalStart := start
		start = time.Now()
		response := contextEnvelope(t, result, err)
		serialization = append(serialization, time.Since(start))
		total = append(total, time.Since(totalStart))
		if i != 0 {
			continue
		}
		r.ResponseBytes = len(response)
		r.ResponseTokens = (len(response) + 3) / 4
		assert.False(t, strings.Contains(string(response), "FOREIGN_PRIVATE_EVIDENCE"))
		values := evidenceValues(t, result)
		for _, wanted := range task.Evidence {
			if values[wanted] {
				r.Found++
			} else {
				r.Missing = append(r.Missing, wanted)
			}
		}
		expectedOutcome := err == nil
		if task.ExpectError {
			expectedOutcome = errors.Is(err, store.ErrNotFound) || agentctx.IsSuggestion(err)
		}
		r.Complete = r.Found == r.Required && expectedOutcome
	}
	if len(task.Files) > 0 {
		files := map[string]string{}
		for _, name := range task.Files {
			files[name] = f.Files[name]
		}
		r.RawBytes = len(contextEnvelope(t, files, nil))
		r.RawTokens = (r.RawBytes + 3) / 4
	}
	r.Reduction = reductionPercent(r.RawTokens, r.ResponseTokens, r.Complete)
	r.Cold, r.Warm, r.Serialization = latency(cold), latency(warm), latency(serialization)
	r.Total = latency(total)
	return r
}

func TestRetrievalFixtureEvidence(t *testing.T) {
	f, _ := loadRetrievalFixture(t)
	for _, task := range f.Tasks {
		t.Run(task.Name, func(t *testing.T) {
			r := evaluateRetrieval(t, f, task, 1)
			assert.True(t, r.Complete, "missing evidence: %v", r.Missing)
		})
	}
	assert.True(t, reductionPercent(0, 10, true) == nil)
	assert.True(t, reductionPercent(100, 10, false) == nil)
	assert.Equal(t, *reductionPercent(100, 120, true), float64(-20))
}

func TestRetrievalReport(t *testing.T) {
	path := os.Getenv("CODESYNAPSE_RETRIEVAL_REPORT")
	if path == "" {
		t.Skip("set CODESYNAPSE_RETRIEVAL_REPORT to write the reproducible workload report")
	}
	f, raw := loadRetrievalFixture(t)
	cpu := os.Getenv("CODESYNAPSE_BENCH_CPU")
	if cpu == "" {
		cpu = "unspecified; set CODESYNAPSE_BENCH_CPU"
	}
	r := retrievalReport{
		Version: f.Version, RunLabel: os.Getenv("CODESYNAPSE_BENCH_LABEL"), FixtureHash: fmt.Sprintf("%x", sha256.Sum256(raw)), PolicyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(f.Policy))),
		Generated: time.Now().UTC().Format(time.RFC3339), Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPU: cpu, CPUs: runtime.NumCPU(), Procs: runtime.GOMAXPROCS(0), Concurrency: 1,
		Estimator: "ceil(serialized UTF-8 bytes / 4), not a model tokenizer",
		Envelope:  "MCP SDK v1.8.0 default negotiation; structuredContent plus duplicate content text; serverInfo codesynapse/fixture-v1; resultType complete; JSON-RPC id 1; no HTTP/SSE framing",
		Scope:     "synthetic versioned source/metadata fixture; in-process memory store; fresh-store cold excludes construction and is not OS cache cold; warm excludes envelope serialization (reported separately); evidence coverage is exact fixture facts, not LLM answer quality; no auth, network, embedding model or Milvus benchmark; null reduction means no baseline or incomplete evidence; successful results include both SDK structuredContent and duplicate text content in both baselines; shared one-time tool schemas/system prefix and HTTP/SSE transport framing excluded",
	}
	r.SourceFiles, r.GraphNodes = len(f.Files), 1+len(f.Files)+len(f.Nodes)+1
	for _, contents := range f.Files {
		r.SourceBytes += len(contents)
	}
	for _, task := range f.Tasks {
		result := evaluateRetrieval(t, f, task, 100)
		r.Tasks = append(r.Tasks, result)
		if result.Complete && result.RawTokens > 0 {
			r.RawTokens += result.RawTokens
			r.ResponseTokens += result.ResponseTokens
		}
	}
	r.Reduction = reductionPercent(r.RawTokens, r.ResponseTokens, true)
	r.Memory = measureMemoryRetrieval(t, 100)
	encoded, err := json.MarshalIndent(r, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(encoded, '\n'), 0o600))
	t.Logf("report: %s; complete raw tokens %d; response tokens %d; lexical p95 %.3f ms", path, r.RawTokens, r.ResponseTokens, r.Memory.Lexical.P95)
}

type memoryRetrievalReport struct {
	Entities          int                       `json:"entities"`
	Query             string                    `json:"query"`
	K                 int                       `json:"k"`
	Lexical           latencyReport             `json:"lexical"`
	Fusion            latencyReport             `json:"fusion_synthetic_dense"`
	ThresholdMS       int                       `json:"migration_threshold_ms"`
	ThresholdExceeded bool                      `json:"migration_threshold_exceeded"`
	LexicalFound      bool                      `json:"lexical_expected_entity_found"`
	DenseFound        bool                      `json:"synthetic_dense_expected_entity_found"`
	FusedFound        bool                      `json:"fused_expected_entity_found"`
	Quality           map[string]fixtureQuality `json:"synthetic_quality_at_k"`
	Note              string                    `json:"note"`
}

type fixtureQuality struct {
	Returned  int     `json:"returned"`
	Relevant  int     `json:"relevant"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
}

func memoryQuality(ids []string, relevant string) fixtureQuality {
	q := fixtureQuality{Returned: len(ids)}
	if slices.Contains(ids, relevant) {
		q.Relevant, q.Recall = 1, 1
	}
	if len(ids) > 0 {
		q.Precision = float64(q.Relevant) / float64(len(ids))
	}
	return q
}

func TestMemoryRetrievalQuality(t *testing.T) {
	r := measureMemoryRetrieval(t, 1)
	assert.True(t, r.LexicalFound)
	assert.True(t, r.DenseFound)
	assert.True(t, r.FusedFound)
	assert.Equal(t, r.Quality["lexical"].Precision, float64(1))
	assert.Equal(t, r.Quality["dense"].Precision, .5)
	assert.Equal(t, r.Quality["fused"].Recall, float64(1))
	assert.Equal(t, memoryQuality(nil, "absent").Recall, float64(0))
}

func measureMemoryRetrieval(t *testing.T, samples int) memoryRetrievalReport {
	t.Helper()
	entities := make([]mem.Entity, mem.MaxEntitiesPerRepo)
	for i := range entities {
		entities[i] = mem.Entity{ID: fmt.Sprintf("entity-%04d", i), Repo: "org-fixture|payments", Name: fmt.Sprintf("service-%04d invoice worker", i), Type: "Component"}
	}
	entities[42].Name = "payments settlement E-PAY.42"
	entities[42].Aliases = []string{"Faktura 支払い"}
	dense := []mem.Scored{{Entity: entities[42], Score: .9}, {Entity: entities[45], Score: .8}}
	r := memoryRetrievalReport{Entities: len(entities), Query: "payments settlement", K: 8, ThresholdMS: 50, Quality: map[string]fixtureQuality{}, Note: "BM25 over 5,000 short names, aliases and types; Go RRF k=60 over real lexical results and fixed synthetic dense candidates; one relevant fixture entity; no embedding latency, model accuracy, vector backend, graph hydration or session-memory measurement"}
	var lexicalTimes, fusionTimes []time.Duration
	for range samples {
		start := time.Now()
		lexical := mem.RankBM25(entities, r.Query, 8)
		lexicalTimes = append(lexicalTimes, time.Since(start))
		start = time.Now()
		fused := mem.FuseRRF(8, map[string][]mem.Scored{"name": lexical, "vector": dense})
		fusionTimes = append(fusionTimes, time.Since(start))
		r.LexicalFound = len(lexical) > 0 && lexical[0].ID == entities[42].ID
		r.FusedFound = len(fused) > 0 && fused[0].ID == entities[42].ID
		lexicalIDs := make([]string, len(lexical))
		for i, item := range lexical {
			lexicalIDs[i] = item.ID
		}
		fusedIDs := make([]string, len(fused))
		for i, item := range fused {
			fusedIDs[i] = item.ID
		}
		r.Quality["lexical"] = memoryQuality(lexicalIDs, entities[42].ID)
		r.Quality["dense"] = memoryQuality([]string{dense[0].ID, dense[1].ID}, entities[42].ID)
		r.Quality["fused"] = memoryQuality(fusedIDs, entities[42].ID)
		r.DenseFound = r.Quality["dense"].Relevant == 1
	}
	r.Lexical, r.Fusion = latency(lexicalTimes), latency(fusionTimes)
	r.ThresholdExceeded = r.Lexical.P95 > float64(r.ThresholdMS)
	return r
}
