# Task eval

Do the agent tools point at the files a change ends up touching? `codesynapse eval` answers it from git history, with no model involved ([agent workflow roadmap](../../PLAN.md#agent-workflow-roadmap), step 1).

## Method

- **Graph at the base commit.** The repository is checked out at `base` in a temporary worktree and indexed in memory, so no answer can draw on the change it is asked about.
- **One task per later commit** in `base..head`, without merges. The question is the commit subject; the answer is the files it modified or deleted that the graph knew at base. Added files cannot be found and are only counted.
- **Skipped:** dependency updates (`chore(deps)`), reverts, and sweeping commits that touch more than 25 files.
- **`minimal_context`** gets the subject with its default budget of 1,500 tokens.
- **`search`** matches names, so a sentence finds nothing. It is scored the way an agent uses it: each of the first five keywords `minimal_context` extracted is searched once (limit 10), and the results are interleaved by rank.
- **Scores per task:** recall@5 and recall@10 over the distinct files an answer names, in its order; hit@10 (at least one right file); returned tokens as serialized bytes / 4, as in [phase 4f](../../RETRIEVAL_RESULTS.md). Reports show the mean over tasks.
- Runs are deterministic: the same commits and code give byte-identical reports.

## Baseline

[`codesynapse.json`](codesynapse.json): this repository, base `409c577`, head `9f63cae` (2026-10-23), 58 tasks from 107 commits.

| Tool | recall@5 | recall@10 | hit@10 | tokens |
|---|---:|---:|---:|---:|
| `minimal_context` | 0.120 | 0.189 | 0.466 | 1,376 |
| `search` (per keyword) | 0.112 | 0.171 | 0.517 | 799 |

Neither tool names a single touched file in its top ten for about half of the tasks. Two patterns stand out in the per-task results (`--results -o json`):

- Changes here usually also touch the same documents (`AGENTS.md`, `docs/PLAN.md`, `docs/SAAS.md`), and the tools rarely return them for a code task.
- Subjects are short and abstract (`fix(compose): keep HugeGraph auth on restart`); the identifiers an agent would need are often not in them.

These are the numbers later roadmap steps have to move.

**2026-10-24, leaner answers (step 5):** the baseline moved to `minimal_context` 0.120 / 0.182 / 0.500 / 1,349 tokens and `search` 0.112 / 0.171 / 0.517 / 753 tokens. Search answers are 6% smaller with the same results (compact IDs). `minimal_context` still fills its budget, now with more items, so one more task in ten finds a right file (hit@10 0.466 → 0.500); recall@10 moved by −0.007 because a larger, better-ranked item can now fit where two smaller ones used to. The eval now scores the answers through `internal/coretools`, the same functions the MCP server and the bridge use.

The graph at base includes the 500 commits before it, like a default ingest, so signals from history are measured; the commits after base, the tasks, are never in it.

## Running it

```sh
just eval                         # compare with the baseline (fails on a regression)
just eval -o json --results       # every task, with the files each tool returned
go run ./cmd/codesynapse eval ../other-repo --base HEAD~100    # any repository
```

CI runs `just eval --allow-network` (the base may need module versions the cache lacks). A regression is a recall or hit rate more than 0.02 below the baseline, more than 2% more tokens, or more errors.

**Moving the baseline** after a deliberate change, or to a newer window: run `just eval --base <sha> --head <sha> --write-baseline` and commit the file with the reason in the commit message. Keep `--repo codesynapse` (the recipe sets it): node IDs carry the repository name, so it changes token counts.

## Cases from your own sessions

Commit subjects are terse; people ask agents in sentences. To score the tools on the questions you actually ask, start Claude Code with `CODESYNAPSE_EVAL_CAPTURE=true` and the plugin installed, and work as usual. For each session that changes files, the hooks keep a case in `codesynapse-eval-cases.jsonl` in the checkout's git directory: the session's first prompt (secrets and personal numbers redacted, at most 500 characters), the commit it began at, and the files it changed that existed there (files it created are only counted). Nothing is sent anywhere, the file is never committed, and it keeps the newest 500 cases.

`codesynapse eval . --sessions` builds the graph at each case's commit and scores `minimal_context` and `search` exactly like the commit tasks; `--results` lists every case with the files each tool named. Cases of commits this checkout lacks (a rebased branch) are skipped. There is no baseline for them: they are yours, and they change as you work.

