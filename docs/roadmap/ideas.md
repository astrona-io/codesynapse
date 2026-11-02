# Ideas and open work

Work that is proposed, planned or only partly built. The ordered list with the measurements behind it is the [roadmap in Plan and decisions](../PLAN.md#roadmap); the plans hold the details.

| Idea | State | Where |
|---|---|---|
| Show findings of other security tools in the report, MCP answers and dashboard, with a `codesynapse sarif upload` command | Planned (phase 2). Phase 1, upload and storage, is built. | [SARIF import plan](../plans/SARIF_IMPORT_PLAN.md) |
| Run external security tools from CodeSynapse | To discuss (phase 3) | [SARIF import plan](../plans/SARIF_IMPORT_PLAN.md) |
| Platform statistics for administrators: firing time per alert rule | Not built: needs vmalert's remote write | [Platform statistics plan](../plans/PLATFORM_STATS_PLAN.md) |
| tree-sitter parsers for exact Python, JavaScript and TypeScript syntax, with tsc and pyright as optional type enrichers | Open. The languages are covered today by pure-Go structural parsers. | [Roadmap item 4](../PLAN.md#roadmap) |
| Webhooks from the forge, so a push re-indexes at once instead of at the next scheduled sync | Open | [Roadmap item 8](../PLAN.md#roadmap) |
| A second NebulaGraph space for reviews, memory, curated records, usage and snapshots, so the MCP server needs only read access to the code graph | Not done: needs a data migration | [Roadmap item 9](../PLAN.md#roadmap) |

Have an idea? Open an issue on [GitHub](https://github.com/astrona-io/codesynapse/issues). Larger ones get a plan in `docs/plans/` first.
