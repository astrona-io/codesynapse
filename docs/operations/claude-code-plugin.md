# Claude Code plugin

!!! info "In plain words"
    A **plugin** is an add-on for Claude Code. This one installs everything CodeSynapse needs in one step: the connection to the server, a short guide that teaches Claude how to use CodeSynapse well, and **hooks**. A hook is a small program Claude Code runs by itself at a set moment (when a chat starts, before a file is read, when Claude finishes). The table below lists each hook. None of them can break a session: if something goes wrong they stay quiet.

The plugin packages everything CodeSynapse does in Claude Code in one install: the MCP server (through the [bridge](claude-code-bridge.md)), a skill that teaches the tool workflow, and four hooks. It replaces running `codesynapse mcp install` and `codesynapse mcp hooks` by hand.

## Install

Three steps. Do them in order.

1. Install the `codesynapse` binary and put a personal API key where the bridge finds it: [Claude Code bridge](claude-code-bridge.md#install-the-binary). If your server is not on this machine, set `CODESYNAPSE_MCP_URL` and `CODESYNAPSE_API_URL` in your shell profile; the bridge reads them.
2. In Claude Code:

   ```
   /plugin install codesynapse --marketplace astrona-io/codesynapse
   ```

   or, from a shell, `claude plugin marketplace add astrona-io/codesynapse` and `claude plugin install codesynapse@codesynapse`.
3. Run `codesynapse mcp install --only claude` once. With the plugin enabled it registers nothing for Claude Code. Instead it sets the 150k context cap and removes what an earlier `mcp install` or `mcp hooks` set up, so tools and hooks do not run twice: the user-scope `codesynapse` server and the CodeSynapse hooks in `~/.claude/settings.json` and the project's `.claude/settings.local.json`. It only reports hooks in the project's shared `.claude/settings.json`; remove those once your team uses the plugin. Running it again changes nothing.

## What it adds

Most of this is for people who want the detail. The short version is in the table further down.

The Stop hook also reads model usage from the local `transcript_path`: input,
output, cached input and reported assistant turns. It counts duplicate assistant
blocks once and reports only new numeric totals after a successful report.
Claude input includes uncached input, cache reads and cache writes; cached input
is shown as part of input, not added again to the total. Statistics shows the
input/output percentage split and keeps estimated context savings separate.
Transcripts are read locally with a 64 MiB limit; their text, paths and message
IDs are never sent. Missing or unsupported usage remains unavailable. The parser
also accepts Codex cumulative `token_count` events when a hook supplies its
transcript; installing the Claude plugin does not install Codex hooks. Reports
are best effort, like existing retrieval counts, and are not a billing ledger.

PostToolUse and PostToolUseFailure hooks for `Edit|Write` record attempt counts,
failures and payload byte sizes. Stop reports those numbers and clears them only
after success. For successful edits with a known file size, the dashboard compares
old + new text bytes with a hypothetical full-file rewrite, estimating avoided
output at bytes/4. Edits are never blocked. Ruleset version 2 encourages targeted
edits before the agent emits its changes; the hooks measure, they do not shorten
an edit already emitted. Native Codex/Cursor lifecycle hooks also observe compatible
edit metadata and forward a `transcript_path` when supplied by the runtime.

| Part | What it does |
|---|---|
| MCP server `codesynapse` | `codesynapse mcp bridge`: the CodeSynapse tools; every call goes to the server (opt in to working-tree answers with `CODESYNAPSE_BRIDGE_WORKTREE=true`). In Claude Code the tools are named `mcp__plugin_codesynapse_codesynapse__<tool>`. |
| Skill `/codesynapse:codesynapse` | The workflow: `minimal_context` first, `get_symbol_context` before editing, `file_outline` before reading, `change_checklist` before changes that ripple, `verify_plan` and `get_pr_context` before finishing. Claude loads it on its own for work in an indexed repository. |
| SessionStart hook: ruleset | `codesynapse hook session`: puts the CodeSynapse ruleset in the session's context, so agents use the tools before reading files without anyone pasting a prompt. It asks the server (`GET /api/v1/agent-rules`, so the wording can be improved for every machine at once) and falls back to the built-in text when the server cannot be reached. In a checkout of an indexed repository it adds that repository's house style (`GET /api/v1/repos/{repo}/conventions`): how its tests assert, how errors and logs are made, its frameworks. |
| SessionStart hook: memory | `codesynapse memory recall --hook`: recent memory facts of the repository. |
| PreCompact hook | `codesynapse memory offload --hook`: older turns go to background extraction by a local model ([agent memory](../features/memory.md)). |
| PreToolUse hook on Read | `codesynapse hook read`: the first time in a session Claude reads all of a file with 400 or more lines, the read is turned back with the file's outline (Go: every function and type with its lines) and the advice to read only the needed ranges or call `file_outline`. Reading it again is allowed. Ranged reads are never turned back. `CODESYNAPSE_READ_GUARD_LINES` changes the size; `0` turns it off. |
| Every hook above | Each plugin hook command also saves the metadata-only activity of its event (`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `Stop`) through the same endpoint, in the same process, with a 1.5 s limit: the first call registers the chat's session, later ones only post. `CODESYNAPSE_HOOK_ACTIVITY=false` turns it off. A `PreToolUse` on a tool no matcher covers (for example `WebFetch`) is not seen. |
| CwdChanged, PreCompact | Recorded through the lifecycle adapter like the events below (PreCompact also sends unreported counts). |
| SessionEnd, SubagentStart, SubagentStop, PostCompact, PostToolBatch, InstructionsLoaded, PermissionRequest, PermissionDenied, StopFailure | `codesynapse hook lifecycle --client claude`: the same adapter Codex uses. Each event registers the chat's runtime session and saves metadata-only activity (event, outcome, sizes, durations; never prompts, commands, paths or text). SessionEnd and SubagentStop also send the counts the session has not reported yet, so a session that ends without a Stop loses nothing. SubagentStart gives the subagent the ruleset. PostCompact only records: Claude Code ignores its output, and the SessionStart hook restores the rules after a compaction. InstructionsLoaded records how large each CLAUDE.md or rules file is, not its name. PermissionRequest and PermissionDenied only observe; they never decide. |
| PreToolUse hook on Bash | `codesynapse hook bash`: when a command is `git status/diff/log/blame`, a text search (`grep`, `rg`), `cat`/`head`/`tail` of a file or a whole test run, it adds a one-line hint naming the CodeSynapse tool that answers with less output (once per class and session) and counts every one. It never blocks. Only the class is kept and reported, never the command. Codex gets the same hint through its lifecycle adapter. |
| PreToolUse hook on Read, Grep, Glob and the CodeSynapse tools | `codesynapse hook lookup`: notes whether the session's first code lookup was a CodeSynapse tool or a native one. It never blocks. |
| PostToolUse hooks on the CodeSynapse tools and on Read | `codesynapse hook answer` notes the `file:start-end` ranges each answer gives; `codesynapse hook read-done` marks the ones a later Read overlaps. The ranges stay in the session's state file on your machine. |
| UserPromptSubmit pre-brief | `codesynapse hook brief`: before a prompt about code reaches the agent, a local model briefs it. The API names the line ranges the graph finds for the prompt (`POST /api/v1/repos/{repo}/brief-plan`, graph only); the hook reads those ranges from the checkout (tracked files only) and sends them with the prompt to `ask-local` in brief mode, which runs only on the local model routed to `local-answer`. The agent gets the briefing (where the work is, what exists, what to watch, each point cited `file:line`) as context next to your prompt, which reaches it unchanged. Short replies are skipped (the same filter as memory recall), a briefing that cites nothing is dropped, and any failure or the 150 s limit means no briefing: the prompt is never blocked. It waits for the local model, often 30–60 s. `CODESYNAPSE_PREBRIEF=false` turns it off; `CODESYNAPSE_PREBRIEF_SECONDS` changes the limit. Statistics → Models & savings counts the prompts briefed. |
| UserPromptSubmit hook | `codesynapse hook prompt`: only with `CODESYNAPSE_EVAL_CAPTURE=true`, keeps the session's first prompt (redacted, shortened) in its local state, so the Stop hook can save the session as an eval case in the checkout's git directory (`codesynapse eval --sessions`; see [the task eval](../benchmarks/task-eval/README.md)). Nothing is sent. |
| Stop hook | `codesynapse hook stop` first reports, per tool, how many ranges answers gave and how many were read since the last report (tool names and counts only, never paths or the session; to the API at `CODESYNAPSE_API_URL` with the key the bridge uses; a failure is retried at the next stop). The Statistics page shows the share read per tool. It also reports how the session began (CodeSynapse first, or Read or Grep first), how many whole-file reads the guard turned back, and how many files the session changed and how many of them a CodeSynapse answer had named (against the working tree and HEAD noted at SessionStart, so files already changed then do not count; commits made during the session do): counts only. Then, when files CodeSynapse indexes changed since the last check (code, YAML and templates; not Markdown, lock files, JSON or local settings such as `.claude/` and `.mcp.json`), Claude is asked once to run `verify_plan` and the commands it lists and to check `get_pr_context`'s `security_delta` before finishing. Changed Go files are first parsed and checked with gofmt inside the hook (standard library, no command runs); a syntax error or an unformatted file is listed with its location at the top of the message, so Claude fixes it before spending two tool calls. The same tree is never asked about twice, and a stop that is already continuing because of a hook is let through. |

The hooks never fail a session: a problem (no repository, no git, unreadable input) means no opinion. Their state (which files were turned back, which tree was checked) is kept per chat session in the plugin's data folder.

## The ruleset and how to improve it

The built-in ruleset is short (`internal/agentrules`): start with `minimal_context`, `get_symbol_context` before editing, `file_outline` before large reads, `change_checklist` before ripples, `verify_plan` and `get_pr_context` before finishing. To use your own wording, put it in a file (at most 4 KiB) and set `CODESYNAPSE_AGENT_RULES_FILE` on the API; every session picks it up at its next start, with no plugin update.

**Statistics → Agents → Do agents use CodeSynapse first?** shows the share of sessions that began with a CodeSynapse tool, the sessions that began with Read or Grep, and the whole-file reads turned back. Change the wording, then watch whether the first share rises. Without the plugin the same ruleset is still available to paste (`GET /api/v1/agent-rules`) or to write into `AGENTS.md` with `codesynapse mcp agents-md`.

## Check it

`just plugin-check` runs Claude Code's validator (`claude plugin validate`) on the plugin and the marketplace when the `claude` CLI is installed. Its two warnings are deliberate: no `version`, so installs follow the repository's commits, and no icon, since the plugin is not listed in Claude's plugin directory. CI checks the same files with `TestPluginFiles`: they parse, and every command the hooks and the MCP configuration run exists in the CLI.

## Updating

The plugin follows the repository: without a pinned version, `/plugin update` (or Claude Code's automatic updates) takes the latest commit of the marketplace. The `codesynapse` binary updates separately (`brew upgrade codesynapse` once releases are public).
