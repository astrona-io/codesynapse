# CodeSynapse plugin for Claude Code

Puts the CodeSynapse code graph into Claude Code: the MCP bridge (every call goes to the server, so it is audited and counted; working-tree answers are an opt-in), a skill with the tool workflow, and hooks that recall memory at session start, offload older turns before compaction, steer whole reads of large files to an outline, and ask for `verify_plan` and `get_pr_context` once before the session's own changes are called done.

```
/plugin install codesynapse --marketplace astrona-io/codesynapse
```

It needs the `codesynapse` binary on your `PATH`, a CodeSynapse server and a personal API key. After updating the plugin or the CLI, run `codesynapse doctor --fix` and start a new session; a session start tells you when they no longer match. When the server cannot be reached, the first hook that tries marks it down for every chat on the machine for two minutes and the hooks skip it meanwhile, so prompts are not slowed down; `codesynapse doctor` shows the mark and the last pre-brief. Setup, settings and troubleshooting: [docs/operations/claude-code-plugin.md](../../docs/operations/claude-code-plugin.md).
