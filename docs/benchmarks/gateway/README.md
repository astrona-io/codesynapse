# Gateway benchmark

How many of Claude Code's requests would the local model keep, and how fast? `just gateway-bench` answers it for one local model ([how to run it](../../operations/gateway.md#benchmark-the-local-model)).

## Method

- **Requests:** nine synthetic Claude Code requests in `services/api/internal/gateway/testdata/gatewaybench`, made up about a fictional project: two haiku-class background requests (a topic title, a one-token quota check) and seven main-model turns with the full system prompt and about thirty tools, from the first turn through `tool_use`/`tool_result` rounds, a failed build, a passing test run, a question without tools and a 240 KB turn with four large files read.
- **Path:** each request goes through the gateway's own `Local.serve` (translation, the local call and the checks on the held answer), one at a time. No cloud request is made.
- **Measured:** the route (`local`, or `cloud` with the fall-through reason), the time to the model's first content, reasoning or tool-call delta, the total time and the tokens the model reported.
- **Not measured:** whether a kept answer is right. The benchmark counts what the gateway's checks accept, which is what decides the route.

Results files hold numbers and fixture names only.
