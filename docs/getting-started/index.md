# Getting started

Three steps take you from nothing to an assistant that asks the graph instead of reading files.

<div class="grid cards" markdown>

- **1. Run the stack**

    One command starts the API, worker, MCP server, dashboard and the stores.

    [Running the stack](../operations/running.md)

- **2. Create your accounts**

    Sign in as `admin` (random password in `.env`), replace it with your own account, then create an organisation and its owner.

    [First run and sign-in](../operations/first-run.md)

- **3. Connect an agent**

    Register the MCP server with Claude Code, Codex, Cursor and others, then add a repository in the dashboard.

    [Claude Code bridge](../operations/claude-code-bridge.md) · [Agent guide](../AGENT_GUIDE.md)

</div>

## The short version

```sh
just up        # start everything in docker compose
# open http://localhost:3000, sign in as admin (password: grep ADMIN_PASSWORD .env),
# replace the admin with your own account, then create the organisation owner
just mcp       # register the MCP server with your coding agents
```

## Where to go next

| I want to... | Read |
|---|---|
| Understand how it works | [Architecture](../architecture/index.md) |
| See what it can answer | [Features](../features/index.md) |
| Run it for a team | [Administration](../operations/index.md) |
| Look up a tool, setting or plan | [Reference](../reference/index.md) |
