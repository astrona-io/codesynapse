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
(cache reads) and cache writes are kept apart, and a turn counts as
`cache-known` only when its usage carried both cache fields. Statistics shows the
input/output percentage split and the share of the agent's work tokens (fresh
input + cache writes + output, without cache reads) that CodeSynapse answers
avoided; that share is unknown until every counted turn reported its cache
fields.

The hooks also count answered line ranges the session already had (an earlier
answer or `Read` overlapped them), as `avoided_repeat_ranges` per tool, and
`resend_token_turns` per tool: the new part of each answer's tokens times the
turns the session ran after it. Only counts per tool name leave the machine,
never paths, ranges or session IDs. The server scales avoided tokens down by
the repeat share; re-sends feed only the separately labelled upper figure.
Clients that do not send `avoided_repeat_ranges` (older CLIs) are left out of
the share and the saved cost.
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
| MCP server `codesynapse` | `codesynapse mcp bridge`: the CodeSynapse tools; core tools for the checkout are answered from the working tree while it has unsynced changes and reported to the server; everything else goes to the server (`CODESYNAPSE_BRIDGE_WORKTREE=false` sends every call). In Claude Code the tools are named `mcp__plugin_codesynapse_codesynapse__<tool>`. |
| Skill `/codesynapse:codesynapse` | The workflow: `minimal_context` first, `get_symbol_context` before editing, `file_outline` before reading, `change_checklist` before changes that ripple, `verify_plan` and `get_pr_context` before finishing. Claude loads it on its own for work in an indexed repository. |
| SessionStart hook: ruleset | `codesynapse hook session`: puts the CodeSynapse ruleset in the session's context, so agents use the tools before reading files without anyone pasting a prompt. It asks the server (`GET /api/v1/agent-rules`, so the wording can be improved for every machine at once) and falls back to the built-in text when the server cannot be reached. In a checkout of an indexed repository it adds that repository's house style (`GET /api/v1/repos/{repo}/conventions`): how its tests assert, how errors and logs are made, its frameworks. |
| SessionStart hook: memory | `codesynapse memory recall --hook`: recent memory facts of the repository. |
| UserPromptSubmit hook: memory | `codesynapse memory recall --hook`: facts of this chat that match the prompt and were not shown yet. Short replies make no call. Session registration, repository lookup and the lookup by meaning share one 5 s budget, under the hook's 10 s timeout; a recall that runs out of time adds nothing and says nothing. `CODESYNAPSE_AUTO_RECALL=false` turns it off. |
| PreCompact hook | `codesynapse memory offload --hook`: older turns go to background extraction by a local model ([agent memory](../features/memory.md)). |
| PreToolUse hook on Read | `codesynapse hook read`: the first time in a session Claude reads all of a file with 400 or more lines, the read is turned back with the file's outline (Go: every function and type with its lines) and the advice to read only the needed ranges or call `file_outline`. Reading it again is allowed. Ranged reads are never turned back, and neither are files git does not track (a saved tool output, a log, a scratch file, a new file): `ask_local` and the graph cannot answer for those. `CODESYNAPSE_READ_GUARD_LINES` changes the size; `0` turns it off. |
| Every hook above | Each plugin hook command also saves the metadata-only activity of its event (`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `Stop`) through the same endpoint, in the same process, with a 1.5 s limit: the first call registers the chat's session, later ones only post. `CODESYNAPSE_HOOK_ACTIVITY=false` turns it off. A `PreToolUse` on a tool no matcher covers (for example `WebSearch`) is not seen, and `hook webfetch` saves no activity, so nothing about a fetched page leaves the machine. |
| CwdChanged, PreCompact | Recorded through the lifecycle adapter like the events below (PreCompact also sends unreported counts). |
| SessionEnd, SubagentStart, SubagentStop, PostCompact, PostToolBatch, InstructionsLoaded, PermissionRequest, PermissionDenied, StopFailure | `codesynapse hook lifecycle --client claude`: the same adapter Codex uses. Each event registers the chat's runtime session and saves metadata-only activity (event, outcome, sizes, durations; never prompts, commands, paths or text). SessionEnd and SubagentStop also send the counts the session has not reported yet, so a session that ends without a Stop loses nothing. SubagentStart gives the subagent the ruleset. PostCompact only records: Claude Code ignores its output, and the SessionStart hook restores the rules after a compaction. InstructionsLoaded records how large each CLAUDE.md or rules file is, not its name. PermissionRequest and PermissionDenied only observe; they never decide. |
| PreToolUse hook on Bash | `codesynapse hook bash`: when a command is `git status/diff/log/blame`, a text search (`grep`, `rg`), `cat`/`head`/`tail` of a file or a whole test run, it adds a one-line hint naming the CodeSynapse tool that answers with less output (once per class and session) and counts every one. It never blocks. Only the class is kept and reported, never the command. Codex gets the same hint through its lifecycle adapter. |
| PreToolUse hook on WebFetch | `codesynapse hook webfetch`: when `fetch_source` already has the page in its local cache (`os.UserCacheDir()/codesynapse/sources`, looked up read-only by the same normalised-URL key), the first `WebFetch` of it in a session is turned back with a pointer to `fetch_source` and the same URL. An uncached page, a URL `fetch_source` would refuse (not https, credentials, unparsable) and a second `WebFetch` of the same page go through. No network, and the URL is never sent or logged; the session state keeps only the cache key (a hash). Stop reports the number of steers as a count. `CODESYNAPSE_FETCH_STEER=false` turns it off. |
| PreToolUse hook on Read, Grep, Glob and the CodeSynapse tools | `codesynapse hook lookup`: notes whether the session's first code lookup was a CodeSynapse tool or a native one. It never blocks. |
| PostToolUse hooks on the CodeSynapse tools and on Read | `codesynapse hook answer` notes the `file:start-end` ranges each answer gives; `codesynapse hook read-done` marks the ones a later Read overlaps. The ranges stay in the session's state file on your machine. |
| UserPromptSubmit pre-brief | `codesynapse hook brief`: before a prompt about code reaches the agent, a local model briefs it. The API names the line ranges the graph finds for the prompt (`POST /api/v1/repos/{repo}/brief-plan`, graph only); the hook reads those ranges from the checkout (tracked files only) and sends them with the prompt to `ask-local` in brief mode, which runs only on the local model routed to `local-answer`. The agent gets the briefing (where the work is, what exists, what to watch, each point cited `file:line`) as context next to your prompt, which reaches it unchanged. Short replies are skipped (the same filter as memory recall), and so are prompts that are not about code ("start more agents": no identifier, path, error text or task verb aimed at a part of the code; the API plans no ranges for them either). Turns Claude Code starts itself, such as background task notifications and messages from subagents or other sessions, are not briefed either. A briefing that cites nothing or only says nothing in the files is relevant is dropped, and any failure or the 150 s limit means no briefing: the prompt is never blocked. It waits for the local model, often 30–60 s. `CODESYNAPSE_PREBRIEF=false` turns it off; `CODESYNAPSE_PREBRIEF_SECONDS` changes the limit. Statistics → Models & savings counts the prompts briefed. |
| UserPromptSubmit hook | `codesynapse hook prompt`: only with `CODESYNAPSE_EVAL_CAPTURE=true`, keeps the session's first prompt (redacted, shortened) in its local state, so the Stop hook can save the session as an eval case in the checkout's git directory (`codesynapse eval --sessions`; see [the task eval](../benchmarks/task-eval/README.md)). Nothing is sent. |
| Stop hook | `codesynapse hook stop` first reports, per tool, how many ranges answers gave and how many were read since the last report (tool names and counts only, never paths or the session; to the API at `CODESYNAPSE_API_URL` with the key the bridge uses; a failure is retried at the next stop). The Statistics page shows the share read per tool. It also reports how the session began (CodeSynapse first, or Read or Grep first), how many whole-file reads the guard turned back, how many `WebFetch` calls were pointed at `fetch_source`, and how many files the session changed and how many of them a CodeSynapse answer had named (against the working tree and HEAD noted at SessionStart, so files already changed then do not count; commits made during the session do): counts only. Then, when files CodeSynapse indexes changed since the last check (code, YAML and templates; not Markdown, lock files, JSON or local settings such as `.claude/` and `.mcp.json`), Claude is asked once to run `verify_plan` and the commands it lists and to check `get_pr_context`'s `security_delta` before finishing. Only the session's own changes count: files already changed or committed when the session began are left out, and commits the upstream gained since are not taken for local ones. A session whose agent edited files only in other checkouts is not asked about the checkout it started in (the person may be working there). For another worktree of the same repository the agent edited, the request names its files (`verify_plan` with `files`), because calls with no arguments answer for the start checkout. Other repositories and subagent worktrees (`.claude/worktrees/agent-*`, or edits whose hook input carries `agent_id`) are not asked about; a subagent working in the session's own checkout is, since that change is in the tree the tools answer for. Changed Go files are first parsed and checked with gofmt inside the hook (standard library, no command runs); a syntax error or an unformatted file is listed with its location at the top of the message, so Claude fixes it before spending two tool calls. The same tree is never asked about twice, and a stop that is already continuing because of a hook is let through. |

The hooks never fail a session: a problem (no repository, no git, unreadable input) means no opinion. Their state (which files were turned back, which tree was checked) is kept per chat session in the plugin's data folder.

The hooks also skip their guards while the server is offline. The bridge notes when it cannot reach the server. While it does, the read guard lets whole-file reads through, and the Stop hook skips its pre-finish check with a notice. That check runs at the next stop once the server answers. The prompt briefing, memory recall and activity reports are skipped too. A Stop hook that cannot reach the API itself also skips its check, and no hook waits more than 3 seconds to register the session. See [when the server is down](claude-code-bridge.md#when-the-server-is-down).

## The ruleset and how to improve it

The built-in ruleset is short (`internal/agentrules`): start with `minimal_context`, `get_symbol_context` before editing, `file_outline` before large reads, `change_checklist` before ripples, `verify_plan` and `get_pr_context` before finishing. A short answer-style block follows: answer first, no greetings, recaps or closing offers, negations and exact text kept, only changed lines of code in chat, and full sentences for security warnings, irreversible actions, ordered steps, questions to the user and anything persisted. Its effect on output tokens is not measured. To use your own wording, put it in a file (at most 4 KiB) and set `CODESYNAPSE_AGENT_RULES_FILE` on the API; every session picks it up at its next start, with no plugin update.

**Statistics → Agents → Do agents use CodeSynapse first?** shows the share of sessions that began with a CodeSynapse tool, the sessions that began with Read or Grep, and the whole-file reads turned back. Change the wording, then watch whether the first share rises. Without the plugin the same ruleset is still available to paste (`GET /api/v1/agent-rules`) or to write into `AGENTS.md` with `codesynapse mcp agents-md`.

## Check it

`just plugin-check` runs Claude Code's validator (`claude plugin validate`) on the plugin and the marketplace when the `claude` CLI is installed. Its two warnings are deliberate: no `version`, so installs follow the repository's commits, and no icon, since the plugin is not listed in Claude's plugin directory. CI checks the same files with `TestPluginFiles`: they parse, and every command the hooks and the MCP configuration run exists in the CLI.

## Updating

The plugin follows the repository: without a pinned version, `/plugin update` (or Claude Code's automatic updates) takes the latest commit of the marketplace. The `codesynapse` binary updates separately (`brew upgrade codesynapse` once releases are public).

When the two drift apart, the session start says so. The rules the hook fetches name the server's version. A release CLI older than the server gets this, shown to you and handed to the agent to repeat before its first answer:

> You are on an old version of CodeSynapse (v0.0.23; the newest is v0.0.24). Please upgrade to the newest version of CodeSynapse, then run `codesynapse doctor --fix` and start a new session to pick up the missing changes from this update.

A plugin that lacks hooks this CLI ships gets "Please run `codesynapse doctor --fix` to fix the missing changes from this update, then start a new session." A plugin that runs hooks the CLI does not have asks for a CLI upgrade. Development and untagged builds (`dev`, `v0.0.23-4-g…`, `-dirty`) never get a notice. The check reads one local file and adds no request; without a key the hook makes none at all.

## Doctor

`codesynapse doctor` checks the setup and prints one line per check, `OK` or `NEEDS ACTION`, with the command that fixes it:

- **API server**: `CODESYNAPSE_API_URL` (or `--api-url`) answers.
- **CLI version**: this CLI against the server's release. Development builds are not compared.
- **API token**: present (environment or the user config file), the file is mode 0600, and the server accepts it. The key is never printed.
- **MCP registration**: one line per agent `codesynapse mcp install` knows (Claude Code, Codex, Gemini, OpenCode, Cursor, the project's `.mcp.json`). Stale means the binary is gone, the bridge arguments are old or the plugin and a user-scope server are both present.
- **Claude Code plugin**: the installed plugin's hooks against the ones this CLI ships.
- **Gateway failover**: only for a gateway `codesynapse mcp install --gateway` set up. Claude Code should reach it through the local failover proxy, with the hooks that start the proxy and the same key. A setup from before failover existed (Claude Code pointed at the gateway itself) needs action; one made with `--failover=false` is left as it is. An idle proxy is fine: the next session starts it.

`codesynapse doctor --fix` repairs what is safe on this machine: it sets the token file to 0600, re-runs the `mcp install` registration for each missing or stale agent (keeping its URLs), updates a user-scope plugin with `claude plugin marketplace update` and `claude plugin update`, and sets up the gateway's failover proxy again with the gateway URL and key already in `~/.claude/settings.json` (backing the file up), or starts the proxy when Claude Code fell back to the gateway. It never sets up a gateway where there was none. It changes nothing `mcp install` does not write. Upgrading the CLI itself, and a rejected key, stay with you. Start a new session afterwards (restart Claude Code after a plugin update). The command exits non-zero only while something needs action; `-o json` gives the checks as JSON.
