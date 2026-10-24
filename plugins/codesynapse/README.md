# CodeSynapse plugin for Claude Code

Puts the CodeSynapse code graph into Claude Code: the MCP bridge (it answers from your working tree while you have unsynced changes), a skill with the tool workflow, and hooks that recall memory at session start, offload older turns before compaction, steer whole reads of large files to an outline, and ask for `verify_plan` and `get_pr_context` once before a changed tree is called done.

```
/plugin install codesynapse --marketplace astrona-io/codesynapse
```

It needs the `codesynapse` binary on your `PATH`, a CodeSynapse server and a personal API key. Setup, settings and troubleshooting: [docs/operations/claude-code-plugin.md](../../docs/operations/claude-code-plugin.md).
