package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type auditTiming struct {
	Samples       int     `json:"samples"`
	P50           float64 `json:"p50_ms"`
	P95           float64 `json:"p95_ms"`
	MeanAllocated uint64  `json:"mean_allocated_bytes"`
}

type auditCorpus struct {
	Name    string      `json:"name"`
	Hash    string      `json:"corpus_sha256"`
	Policy  string      `json:"policy_digest"`
	Files   int         `json:"files"`
	Bytes   int         `json:"source_bytes"`
	Nodes   int         `json:"nodes"`
	Edges   int         `json:"edges"`
	FirstMS float64     `json:"first_run_ms"`
	Warm    auditTiming `json:"unchanged_reingest"`
	Changed auditTiming `json:"one_file_edit_reingest"`
}

// TestArchitectureAuditReport is opt-in because it runs complete extraction
// repeatedly. It never loads dependencies or writes to a running graph service.
func TestArchitectureAuditReport(t *testing.T) {
	output := os.Getenv("CODESYNAPSE_ARCHITECTURE_REPORT")
	if output == "" {
		t.Skip("set CODESYNAPSE_ARCHITECTURE_REPORT for the phase 4j indexing workload")
	}
	report := struct {
		Version     int           `json:"version"`
		Revision    string        `json:"source_revision"`
		Generated   string        `json:"generated_at"`
		Go          string        `json:"go"`
		OS          string        `json:"os"`
		Arch        string        `json:"arch"`
		CPU         string        `json:"cpu"`
		Procs       int           `json:"gomaxprocs"`
		Concurrency int           `json:"concurrency"`
		Scope       string        `json:"scope"`
		Corpora     []auditCorpus `json:"corpora"`
	}{Version: 1, Revision: os.Getenv("CODESYNAPSE_BENCH_LABEL"), Generated: time.Now().UTC().Format(time.RFC3339), Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPU: os.Getenv("CODESYNAPSE_BENCH_CPU"), Procs: runtime.GOMAXPROCS(0), Concurrency: 1,
		Scope: "Synthetic Go/Python/TypeScript/YAML/Markdown repository; full extraction, ledger preparation, in-memory graph replacement and report validation. No database/network/authentication/model cost. First run is not OS-cache cold; unchanged and edited runs reuse filesystem/toolchain caches and prior identity evidence but rebuild the graph. Allocation deltas include parent Go process only, not go-list/compiler children. Corpus creation and read-memstats are outside timing. Each edit changes one Go file between two values."}
	ctx := logx.WithLogger(t.Context(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, count := range []int{10, 100} {
		files := auditFiles(count)
		encoded, err := json.Marshal(files)
		require.NoError(t, err)
		root := policyFiles(t, files)
		policy, err := repoconfig.Load(ctx, root)
		require.NoError(t, err)
		corpus := auditCorpus{Name: fmt.Sprintf("bundles-%d", count), Hash: fmt.Sprintf("%x", sha256.Sum256(encoded)), Policy: policy.Digest(), Files: len(files)}
		for _, contents := range files {
			corpus.Bytes += len(contents)
		}
		st := memory.New()
		previous := ""
		previousBody, symbolID := "", ""
		iteration := 0
		run := func(edited bool) (float64, uint64) {
			iteration++
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			ledger, err := ingestreport.New(fmt.Sprintf("audit-%d", iteration), start)
			require.NoError(t, err)
			graph, err := Run(ingestreport.WithLedger(ctx, ledger), Options{Root: root, Repo: "audit", PreviousIdentities: previous})
			require.NoError(t, err)
			ledger.Prepare(graph)
			require.NoError(t, st.ReplaceRepo(ctx, "audit", graph))
			final := ledger.Finish(ingestreport.Succeeded, ingestreport.CommitCommitted)
			require.NoError(t, final.Validate())
			elapsed := float64(time.Since(start).Nanoseconds()) / 1e6
			runtime.ReadMemStats(&after)
			require.Equal(t, final.Status, ingestreport.Succeeded)
			require.Equal(t, final.GraphCommit, ingestreport.CommitCommitted)
			require.Equal(t, final.FilesFailed, 0)
			found := false
			for _, candidate := range graph.Nodes() {
				if candidate.Kind != model.KindFunction || candidate.Name != "Work0_0" {
					continue
				}
				found = true
				require.NotEmpty(t, candidate.BodyHash)
				require.NotEmpty(t, candidate.SymbolID)
				if previousBody != "" {
					require.Equal(t, candidate.SymbolID, symbolID)
					if edited {
						require.NotEqual(t, candidate.BodyHash, previousBody)
					} else {
						require.Equal(t, candidate.BodyHash, previousBody)
					}
				}
				previousBody, symbolID = candidate.BodyHash, candidate.SymbolID
			}
			require.True(t, found)
			node, ok := graph.Node(model.NodeID("audit", model.KindRepository, "audit"))
			require.True(t, ok)
			previous = node.Identities
			corpus.Nodes, corpus.Edges = len(graph.Nodes()), len(graph.Edges())
			require.Equal(t, final.TotalFilesScanned, len(files))
			return elapsed, after.TotalAlloc - before.TotalAlloc
		}
		corpus.FirstMS, _ = run(false)
		for _, edited := range []bool{false, true} {
			samples := make([]float64, 0, 20)
			var allocated uint64
			for n := range 20 {
				if edited {
					contents := strings.Replace(files["source_000.go"], "return 1", fmt.Sprintf("return %d", n%2+2), 1)
					require.NoError(t, os.WriteFile(filepath.Join(root, "source_000.go"), []byte(contents), 0600))
				}
				ms, bytes := run(edited)
				samples = append(samples, ms)
				allocated += bytes
			}
			slices.Sort(samples)
			measured := auditTiming{Samples: len(samples), P50: samples[9], P95: samples[18], MeanAllocated: allocated / uint64(len(samples))}
			if edited {
				corpus.Changed = measured
			} else {
				corpus.Warm = measured
			}
		}
		report.Corpora = append(report.Corpora, corpus)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(output, append(encoded, '\n'), 0600))
	t.Logf("architecture report: %s", output)
}

func auditFiles(bundles int) map[string]string {
	files := map[string]string{"go.mod": "module example.com/architecture-audit\n\ngo 1.27.3\n"}
	for n := range bundles {
		var source strings.Builder
		source.WriteString("package audit\n")
		for k := range 10 {
			fmt.Fprintf(&source, "func Work%d_%d() int { return 1 }\n", n, k)
		}
		files[fmt.Sprintf("source_%03d.go", n)] = source.String()
		files[fmt.Sprintf("worker_%03d.py", n)] = fmt.Sprintf("def work_%d():\n    return 1\n", n)
		files[fmt.Sprintf("client_%03d.ts", n)] = fmt.Sprintf("export function work_%d(): number { return 1; }\n", n)
		files[fmt.Sprintf("config_%03d.yaml", n)] = fmt.Sprintf("service:\n  name: worker-%d\n", n)
		files[fmt.Sprintf("guide_%03d.md", n)] = fmt.Sprintf("# Worker %d\n\nA synthetic worker for the indexing audit.\n", n)
	}
	return files
}

func TestArchitectureAuditCorpus(t *testing.T) {
	files := auditFiles(1)
	require.Equal(t, len(files), 6)
	root := policyFiles(t, files)
	ctx := logx.WithLogger(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	graph, err := Run(ctx, Options{Repo: "audit", Root: root})
	require.NoError(t, err)
	functions := 0
	for _, node := range graph.Nodes() {
		if node.Kind == model.KindFunction {
			functions++
		}
	}
	require.Equal(t, functions, 12)
}
