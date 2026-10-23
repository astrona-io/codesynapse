# Phase 4j.2 preflight measurements

Measured 2026-10-23 on Apple M5 Pro, 48 GiB RAM, darwin/arm64, Go 1.27.3,
`GOMAXPROCS=4`, without the race detector. The base revision is recorded in
`manifest.json`, alongside hashes of the implementation and harness. Fixtures
are generated code, Unicode and escaping-heavy strings; no repository source
or conversation data is included.

The measurement includes existing secret/personal-data redaction, budget checks
and canonical request encoding. It excludes provider calls, network and database
work. Three runs use 200 ms per benchmark; ranges below are run means, not p95.

| Workload | Time/op | Allocated bytes/op |
|---|---:|---:|
| Small code, 536 combined prompt bytes | 98.38–100.43 µs | 18,713–18,745 |
| Code, 32,792 combined bytes | 6.75–6.92 ms | 1,115,572–1,121,377 |
| Unicode | 2.97–3.09 ms | 1,084,828–1,092,798 |
| Escaping-heavy text | 2.13–2.23 ms | 756,720–772,776 |
| Registry construction plus tiny first request | 1.98–2.03 µs | 2,793 |
| Parallel 32 KiB code, four workers | 1.99–2.19 ms/op aggregate | 1,141,098–1,150,920 |

Parallel time is amortized throughput, not individual request latency. Registry
construction runs in a warm process, so it does not measure OS-cold startup or
package initialization. There are no vocabulary assets or native tokenizer
resources to initialize. The entire benchmark process reached 37,814,272 bytes
maximum RSS (36.1 MiB); that includes the Go runtime, all fixtures and parallel
work, and cannot be attributed solely to the counter. Allocation volume is not
retained heap.

These figures establish the cost of this estimate implementation. No exact
tokenizer was installed, timed or compared for accuracy. The [budget policy](../../PROMPT_BUDGETS.md)
describes what the estimate admits and when exact-only requests are refused.

## Reproduce

Run from the repository root on a quiet host, separately from builds and tests:

```sh
go test -c -o /tmp/codesynapse-preflight.test ./services/api/internal/llm
GOMAXPROCS=4 /usr/bin/time -l /tmp/codesynapse-preflight.test \
  -test.run '^$' -test.bench '^BenchmarkPromptPreflight$' \
  -test.benchmem -test.benchtime=200ms -test.count=3
```

`/usr/bin/time -l` is the macOS command; use the host equivalent elsewhere.
`preflight.txt` contains the complete benchmark/process output. Recheck toolchain,
CPU and harness hashes before comparing later runs. The implementation uses no
runtime downloads or model endpoint, including for the registry-startup case.
