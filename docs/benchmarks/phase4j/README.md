# Phase 4j audit measurements

These artifacts support the [architecture assessment](../../CODE_ARCHITECTURE_AUDIT.md).
They measure the existing implementation at `ddf78a3`, with the test-only harnesses
added in commit `5c20843063e18d546f3e771a8b11618c9438e51d`. Later fixes change some regression expectations; use that commit to reproduce these historical harness hashes. `manifest.json` records their SHA-256 hashes and the full
base revision. All corpora are generated fixtures; no customer source is included.
Measurements were collected on 2026-10-23, Go 1.27.3, darwin/arm64, Apple M5 Pro,
48 GiB RAM. Benchmark timings have one caller and no race detector.

| Artifact | Scope |
|---|---|
| `indexing.json` | Full synthetic extraction, identity reconciliation, report ledger and memory-store publication; 20 samples per warm/edit series, four processors |
| `indexing-process.txt` | Test-process elapsed time and macOS maximum RSS; no aggregate child/native memory claim |
| `indexing-alloc.txt` | Whole test-process sampled allocation space, including fixture and assertion work outside timed sections |
| `parsing.txt` | Three runs of three iterations, default 18 processors; Go extraction and narrower Python/TS parser boundaries |
| `typescript-cpu.txt` | Separate TypeScript parser CPU profile |
| `diff.txt` | Diff/rewrite microbenchmarks, 200 ms per run, three runs, 18 processors |
| `context.txt` | `minimal_context` synthetic memory graphs, one second per run, three runs, four processors |
| `context-cpu.txt`, `context-alloc.txt` | Separate three-second context profile at 18 processors; includes one fixture setup |
| `retrieval.json` | Unchanged phase 4f report harness at four processors; corpus/policy hashes and token estimates embedded |

`MB` in the assessment means decimal bytes; RSS also gives MiB explicitly.
Allocated bytes are cumulative allocation volume, not retained heap. Microbenchmark
ranges are run means, not p95. Indexing p95 is the nineteenth of twenty sorted
samples, an exploratory estimate. Source bodies are tiny: the larger indexing
corpus has 501 files but only 50,209 bytes. First-run OS/toolchain cache state is
unknown. No provider, remote database, live MCP or concurrent-tenant SLA is measured.

## Reproduce

Run from the repository root on a quiet host. Keep benchmark runs separate from
tests, builds and one another. Record host/toolchain changes before comparing.
The report tests are opt-in; normal `just test` runs the small corpus and gap tests.

```sh
go test -c -o /tmp/codesynapse-audit.test ./internal/ingest
CODESYNAPSE_ARCHITECTURE_REPORT=/tmp/indexing.json \
CODESYNAPSE_BENCH_CPU='describe CPU and RAM' \
CODESYNAPSE_BENCH_LABEL="$(git rev-parse HEAD)" \
GOMAXPROCS=4 /usr/bin/time -l /tmp/codesynapse-audit.test \
  -test.run '^TestArchitectureAuditReport$' -test.count=1 -test.v \
  -test.cpuprofile=/tmp/indexing.cpu -test.memprofile=/tmp/indexing.mem
go tool pprof -top -alloc_space /tmp/codesynapse-audit.test /tmp/indexing.mem

go test ./internal/ingest/golang ./internal/ingest/lang -run '^$' \
  -bench BenchmarkAudit -benchmem -benchtime=3x -count=3
go test ./internal/ingest/lang -run '^$' \
  -bench 'BenchmarkAuditStructuralParse/typescript/functions_1000$' \
  -benchtime=2s -count=1 -cpuprofile=/tmp/typescript.cpu -o /tmp/lang.test
go tool pprof -top /tmp/lang.test /tmp/typescript.cpu

go test ./internal/gitx ./services/api/internal/fixes -run '^$' \
  -bench Audit -benchmem -benchtime=200ms -count=3
GOMAXPROCS=4 go test ./internal/agentctx -run '^$' \
  -bench '^BenchmarkAuditMinimalContext$' -benchmem -benchtime=1s -count=3
GOMAXPROCS=18 go test ./internal/agentctx -run '^$' \
  -bench '^BenchmarkAuditMinimalContext/large$' -benchtime=3s -count=1 \
  -cpuprofile=/tmp/context.cpu -memprofile=/tmp/context.mem -o /tmp/context.test
go tool pprof -top /tmp/context.test /tmp/context.cpu
go tool pprof -top -alloc_space /tmp/context.test /tmp/context.mem

GOMAXPROCS=4 CODESYNAPSE_BENCH_CPU='describe CPU and RAM' \
CODESYNAPSE_BENCH_LABEL="$(git rev-parse HEAD)" \
CODESYNAPSE_RETRIEVAL_REPORT=/tmp/retrieval.json \
go test ./internal/metrics -run '^TestRetrievalReport$' -count=1 -v
```

`/usr/bin/time -l` is the macOS form; use the host's equivalent on other systems.
Indexing validates successful publication, zero failed files, changed body hashes
after edits and stable permanent IDs. It rebuilds the graph on every iteration;
calling an edited run “incremental” would be inaccurate. The initial source-map
hash identifies each generated corpus; edits alternate one function's return value.
The empty indexing policy digest means no repository configuration file.

Raw binary profiles and test executables stay outside Git. Text profile summaries
are retained here for review. Candidate parsers, diff engines and tokenizers were
researched but not installed or timed; no comparative performance claim is made.
