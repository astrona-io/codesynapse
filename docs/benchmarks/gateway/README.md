# Gateway benchmark

How many of Claude Code's requests would the local model keep, and how fast? `just gateway-bench` answers it for one local model ([how to run it](../../operations/gateway.md#benchmark-the-local-model)).

## Method

- **Requests:** nine synthetic Claude Code requests in `services/api/internal/gateway/testdata/gatewaybench`, made up about a fictional project: two haiku-class background requests (a topic title, a one-token quota check) and seven main-model turns with the full system prompt and about thirty tools, from the first turn through `tool_use`/`tool_result` rounds, a failed build, a passing test run, a question without tools and a 240 KB turn with four large files read.
- **Path:** each request goes through the gateway's own `Local.serve` (translation, the local call and the checks on the held answer), one at a time. No cloud request is made.
- **Measured:** the route (`local`, or `cloud` with the fall-through reason), the time to the model's first content, reasoning or tool-call delta, the total time and the tokens the model reported.
- **Not measured:** whether a kept answer is right. The benchmark counts what the gateway's checks accept, which is what decides the route.

Results files hold numbers and fixture names only.

## Baseline

[`qwen3.8-27b-ollama.json`](qwen3.8-27b-ollama.json): Ollama `qwen3.8:27b` (MLX runner, `context_window: 262144`, `effort: none`), main at `3931a3a`, 2026-10-30, `timeout_seconds` 300, one run.

| Fixture | Route | First token ms | Total ms | In tok | Out tok |
|---|---|---:|---:|---:|---:|
| small-title | local | 21,862 | 23,454 | 104 | 27 |
| small-quota | local | 51,984 | 51,985 | 13 | 1 |
| main-first-turn | local | 75,923 | 79,908 | 6,480 | 134 |
| main-after-grep | local | 46,948 | 49,513 | 6,673 | 154 |
| main-after-read | local | 11,029 | 13,303 | 7,778 | 119 |
| main-tool-error | local | 46,240 | 48,564 | 8,105 | 92 |
| main-final-answer | local | 2,365 | 5,857 | 8,103 | 135 |
| main-question | local | 34,560 | 44,882 | 6,465 | 67 |
| main-large-context | cloud (`answer_timeout`) | - | 300,004 | - | - |

8 of 9 answered locally (89%). The model served other agents' requests during the run (a five-token request outside the benchmark took 109 s just before it), so the times include waiting inside Ollama and are an upper bound; the routes are the result to compare. The 240 KB turn (about 60k tokens) did not get a first token within 300 s.
