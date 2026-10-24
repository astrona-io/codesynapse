# Claude Code plugin

The plugin packages everything CodeSynapse does in Claude Code in one install: the MCP server (through the [bridge](claude-code-bridge.md)), a skill that teaches the tool workflow, and four hooks. It replaces running `codesynapse mcp install` and `codesynapse mcp hooks` by hand.

## Install

1. Install the `codesynapse` binary and put a personal API key where the bridge finds it: [Claude Code bridge](claude-code-bridge.md#install-the-binary). If your server is not on this machine, set `CODESYNAPSE_MCP_URL` and `CODESYNAPSE_API_URL` in your shell profile; the bridge reads them.
2. In Claude Code:

   ```
   /plugin install codesynapse --marketplace astrona-io/codesynapse
   ```

   or, from a shell, `claude plugin marketplace add astrona-io/codesynapse` and `claude plugin install codesynapse@codesynapse`.
3. If you registered CodeSynapse before with `codesynapse mcp install` or `codesynapse mcp hooks`, remove that registration (`claude mcp remove codesynapse`) and the hooks from your settings, so tools and hooks do not run twice.

## What it adds

| Part | What it does |
|---|---|
| MCP server `codesynapse` | `codesynapse mcp bridge`: the CodeSynapse tools; every call goes to the server (opt in to working-tree answers with `CODESYNAPSE_BRIDGE_WORKTREE=true`). In Claude Code the tools are named `mcp__plugin_codesynapse_codesynapse__<tool>`. |
| Skill `/codesynapse:codesynapse` | The workflow: `minimal_context` first, `get_symbol_context` before editing, `file_outline` before reading, `change_checklist` before changes that ripple, `verify_plan` and `get_pr_context` before finishing. Claude loads it on its own for work in an indexed repository. |
| SessionStart hook: ruleset | `codesynapse hook session`: puts the CodeSynapse ruleset in the session's context, so agents use the tools before reading files without anyone pasting a prompt. It asks the server (`GET /api/v1/agent-rules`, so the wording can be improved for every machine at once) and falls back to the built-in text when the server cannot be reached. |
| SessionStart hook: memory | `codesynapse memory recall --hook`: recent memory facts of the repository. |
| PreCompact hook | `codesynapse memory offload --hook`: older turns go to background extraction by a local model ([agent memory](../features/agent-memory.md)). |
| PreToolUse hook on Read | `codesynapse hook read`: the first time in a session Claude reads all of a file with 400 or more lines, the read is turned back with the file's outline (Go: every function and type with its lines) and the advice to read only the needed ranges or call `file_outline`. Reading it again is allowed. Ranged reads are never turned back. `CODESYNAPSE_READ_GUARD_LINES` changes the size; `0` turns it off. |
| PreToolUse hook on Read, Grep, Glob and the CodeSynapse tools | `codesynapse hook lookup`: notes whether the session's first code lookup was a CodeSynapse tool or a native one. It never blocks. |
| PostToolUse hooks on the CodeSynapse tools and on Read | `codesynapse hook answer` notes the `file:start-end` ranges each answer gives; `codesynapse hook read-done` marks the ones a later Read overlaps. The ranges stay in the session's state file on your machine. |
| Stop hook | `codesynapse hook stop` first reports, per tool, how many ranges answers gave and how many were read since the last report (tool names and counts only, never paths or the session; to the API at `CODESYNAPSE_API_URL` with the key the bridge uses; a failure is retried at the next stop). The Statistics page shows the share read per tool. It also reports how the session began (CodeSynapse first, or Read or Grep first) and how many whole-file reads the guard turned back: counts only. Then, when the working tree changed since the last check, Claude is asked once to run `verify_plan` and the commands it lists and to check `get_pr_context`'s `security_delta` before finishing. The same tree is never asked about twice, and a stop that is already continuing because of a hook is let through. |

The hooks never fail a session: a problem (no repository, no git, unreadable input) means no opinion. Their state (which files were turned back, which tree was checked) is kept per chat session in the plugin's data folder.

## The ruleset and how to improve it

The built-in ruleset is short (`internal/agentrules`): start with `minimal_context`, `get_symbol_context` before editing, `file_outline` before large reads, `change_checklist` before ripples, `verify_plan` and `get_pr_context` before finishing. To use your own wording, put it in a file (at most 4 KiB) and set `CODESYNAPSE_AGENT_RULES_FILE` on the API; every session picks it up at its next start, with no plugin update.

**Statistics → Agents → Do agents use CodeSynapse first?** shows the share of sessions that began with a CodeSynapse tool, the sessions that began with Read or Grep, and the whole-file reads turned back. Change the wording, then watch whether the first share rises. Without the plugin the same ruleset is still available to paste (`GET /api/v1/agent-rules`) or to write into `AGENTS.md` with `codesynapse mcp agents-md`.

## Check it

`just plugin-check` runs Claude Code's validator (`claude plugin validate`) on the plugin and the marketplace when the `claude` CLI is installed. Its two warnings are deliberate: no `version`, so installs follow the repository's commits, and no icon, since the plugin is not listed in Claude's plugin directory. CI checks the same files with `TestPluginFiles`: they parse, and every command the hooks and the MCP configuration run exists in the CLI.

## Updating

The plugin follows the repository: without a pinned version, `/plugin update` (or Claude Code's automatic updates) takes the latest commit of the marketplace. The `codesynapse` binary updates separately (`brew upgrade codesynapse` once releases are public).
