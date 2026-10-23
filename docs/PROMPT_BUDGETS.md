# Final prompt budgets

Phase 4j.2 checks every completion through `llm.Registry.Complete` before calling
a provider. `Registry.Preflight` exposes the same check for task assembly without
sending a request or recording usage. Execution identity remains mandatory.

## Configure admission

```yaml
providers:
  - name: local-model
    kind: openai
    base_url: http://localhost:9080/v1
    model: your-local-model
    context_window: 32768
    max_output_tokens: 4096
    max_input_tokens: 20000
    input_budget_policy: estimate
```

`estimate` is the default. The current counter uses **one canonical request JSON
byte per estimated input token**. This is deliberately more conservative than
the previous four-bytes-per-token packing heuristic. It counts the final redacted
system and user messages, JSON escaping and all fields in the provider's request
shape. OpenAI-compatible options and Anthropic system cache controls, message
blocks, fallback/effort settings and the SDK's stream flag are included. Shared
request builders keep counting and dispatch aligned.

This is an estimate, not a tokenizer. Server-side templates, model revisions,
special tokens and Anthropic fallback models remain unverified. An accepted
estimate cannot guarantee that an arbitrary model's context window will fit.
It may also refuse a request that the model could accept. The counter never
selects an OpenAI encoding merely because an endpoint is OpenAI-compatible.

Set `input_budget_policy: require_exact` when estimates are unacceptable. **No
verified exact counter is installed in this release, so this policy refuses all
completion calls before network dispatch.** It does not download tokenizers,
call a remote token-count endpoint or silently fall back to estimates. New
tokenizer dependencies and model/template adapters require their own approval
and validation; the phase 4j package comparisons remain candidates.

The AI provider panel displays the policy and labels the input budget as an
estimate. The provider-list API adds `input_budget_policy`; existing usage fields
continue to mean provider-reported usage.

## Enforcement order and limits

1. Validate execution identity, cancellation and the configured budget/policy.
2. Check combined system/user text against a 1 MiB cap and require valid UTF-8.
3. Redact secrets/personal data and check the byte cap again; redaction can grow
   the text, so counting the unredacted prompt would be insufficient.
4. Clamp requested output tokens to the configured maximum, then construct and
   count the complete canonical provider envelope. Its cap is 4 MiB.
5. Enforce the policy and input limit, reserving the requested output plus a
   further 1,024 tokens for unverified provider framing when a context window is
   configured. Dispatch only the prepared, redacted request that passed.

Configured input defaults to the context window minus maximum output and the
1,024-token reserve, or 32,000 without a window. Explicit input budgets are
clamped to that available space. An impossible small window is rejected instead
of falling back to 32,000. Negative budgets, unknown policy names and timeouts
outside 0–86,400 seconds are invalid; zero selects the existing defaults.
Configured model names and effort strings are bounded before serialization.

`InputCount` records the configured model, encoding `utf8-bytes`, version
`request-json-v1`, estimated token count, canonical envelope bytes and
`exact=false`. It is returned by preflight and attached to completion results.
Canonical bytes are not HTTP traffic measurements. `BudgetError` exposes only
counts/limits, never prompt text; raw byte-limit errors report the first value
known to exceed the cap rather than retaining the oversized text.

Rejected requests do not call the provider or increment model request/usage/cost
counters. Admission metadata never replaces reported input/output/cache tokens
or configured-price accounting. Normal provider attempts retain existing usage
and telemetry behavior. Embeddings keep their separate bounded local-only path;
this phase governs completion requests.

## Task behavior and compatibility

Triage, code reviews and briefings preflight complete evidence units while
packing prompts. They no longer admit an oversized first finding or symbol.
Triage advances by the findings actually included in a request, so a budget
split carries remaining findings into the next batch instead of dropping them.
Briefings omit additions that do not fit. Findings and source blocks are never
cut in the middle to satisfy the budget.

All completion callers, including annotation, fixes and memory extraction, pass
through the central check even if they have their own earlier limits. Previously
accepted large prompts may now be refused. Reduce the selected work or configure
a budget appropriate to the model and workload; increasing a limit does not make
the estimate exact. There is no bypass flag for over-budget completions.

Provider configuration is copied at registry creation so later mutation of a
caller's config cannot make preflight and dispatch use different request shapes.
The counter interface is internal. Future adapters must report valid counts and
the matching model/encoding/version, include the complete message/template/tool
framing, preserve byte limits and pass reference fixtures before claiming exactness.

## Validation and measurements

Tests cover exact admission boundaries, system/message overhead, Unicode, long
inputs, special-token literals, JSON escaping, redaction growth, output reserves,
invalid configuration, missing identity, cancellation and concurrent use. Mock
OpenAI-compatible and Anthropic servers verify that the counter describes the
complete canonical payload actually dispatched. They verify request formatting,
not real-model token accuracy. Rejected calls have zero provider invocations;
reported usage remains distinct from the estimate.

[Reproducible benchmark results](benchmarks/phase4j2/README.md) measure redaction
plus preflight, registry construction and parallel calls. They do not establish
a model SLA, tokenizer accuracy or savings from provider caching. No dependency,
vocabulary asset, graph migration or deployment change is required.
