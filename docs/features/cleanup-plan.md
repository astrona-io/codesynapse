# Cleanup plan

CodeSynapse already finds a lot: unreachable code, unused and vulnerable dependencies, licence conflicts, duplicate code and complex files nobody tests. The cleanup plan puts them in one list, ranked by what pays off most for the least risk, so the answer to "what should we fix next?" is a page and not a project.

It only reads what the last ingest stored. It changes nothing.

## Steps

| Kind | What it says | Typical safety |
|---|---|---|
| Delete risky dead code | Unreachable functions that touch exec, SQL, crypto or similar, grouped by directory | high |
| Delete dead code | Unreachable, unexported functions with high confidence, grouped by directory | high |
| Delete a dead file | A non-test file in which nothing is live | medium |
| Remove a dependency | A direct module nothing imports, with the advisories removing it clears | high |
| Upgrade a dependency | A vulnerable module, to the fixed version; for an indirect one, the package that pulls it in (from the dependency path) | medium |
| Licence conflict | A non-development dependency whose licence conflicts with the project's: a flag for whoever owns licensing | low |
| Merge copies | Exact duplicate functions, with how to merge them by how far apart they are | medium |
| Review edited copies | Copies that drifted apart: look at what differs before merging | low |
| Add tests | One of the top hotspots (complexity times churn) that no test reaches | medium |

Left out on purpose, and counted in the notes: exported, test-only and low-confidence unused functions (they may have users outside the repository), advisories of low severity that your code does not call, and licences that only need a read.

## The order

```
priority = value x safety factor / effort divisor
```

Value (0 to 100) is how much the step is worth: a vulnerability your code calls scores 100 at most, risky dead code about 75 to 90, an unused dependency 35 plus 15 per advisory, copies and tests less. Safety factors are 1.0, 0.8 and 0.6 for high, medium and low; effort divisors are 1.0, 1.5 and 2.0 for small, medium and large. Value, effort and safety are shown on every step, so another order is a judgement you can make yourself. The same ingest gives the same plan.

## Where

- **Web**: Code health, Plan.
- **MCP**: `cleanup_plan` (optionally `kind` and `limit`). An agent can take the first step, check it with the listed verification, and report back.
- **API**: `GET /api/v1/repos/{repo}/cleanup/plan`.

A repository indexed before some reports existed gets a partial plan and a note to Sync.
