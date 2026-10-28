# CodeSynapse plugin for Claude Code

Puts the CodeSynapse code graph into Claude Code: the MCP bridge (every call goes to the server, so it is audited and counted; working-tree answers are an opt-in), a skill with the tool workflow, and hooks that recall memory at session start, offload older turns before compaction, steer whole reads of large files to an outline, and ask for `verify_plan` and `get_pr_context` once before the session's own changes are called done.

```
/plugin install codesynapse --marketplace astrona-io/codesynapse
```

It needs the `codesynapse` binary on your `PATH`, a CodeSynapse server and a personal API key. Setup, settings and troubleshooting: [docs/operations/claude-code-plugin.md](../../docs/operations/claude-code-plugin.md).
