# CodeSynapse as a multi-tenant product

This is the target architecture for running CodeSynapse as an open-source, self-hostable SaaS product on Kubernetes. It is built in phases; each phase is a pull request that works on its own. Decisions below were agreed on 2026-10-22.

## Who it is for

Engineering teams and the people around them (team leads, security and compliance, platform owners) who are not AI or data specialists. They know that model tokens cost money, that security issues and vulnerable dependencies matter, and that the wrong licence can be expensive. The product answers in that language: what needs attention, what it costs, and what to do next.

## Concepts

| Concept | Meaning |
|---|---|
| **User** | A person with an email address. Logs in with a password (argon2id) or single sign-on (OpenID Connect). |
| **Organisation** | The tenant. Owns repositories, reviews, agent memory, statistics and settings. A user can belong to several. |
| **Membership** | A user's role in an organisation: `owner`, `admin`, `member`, `viewer`. |
| **API key** | A personal key (`csk_…`) bound to one user and one organisation. Agents (MCP), hooks and the CLI use it. Shown once, stored only as a SHA-256 hash, revocable, with last-used time. |
| **Platform admin** | Operates the installation: sees cross-tenant statistics, manages organisations and users. The first account created becomes one. |
| **Repository** | An organisation's view of a code base (remote, branch, schedule, settings). |
| **Graph** | The indexed code graph of a repository at a commit, in Neo4j. Shared between organisations when allowed (below). |

## Data

- **PostgreSQL** holds relational, tenant-owned data: users, organisations, memberships, sessions, API keys, invitations, settings, audit log, and (from phase 2) the repository registry and reviews.
- **Neo4j** keeps code graphs, agent memory and usage counters, scoped by organisation from phase 2.
- **No source bodies** are stored anywhere, as before (AGENTS.md).

### De-duplication

The same repository is indexed once, not once per organisation:

- A graph is keyed by its remote and commit. Organisations reference graphs through their repository entries.
- **Public repositories** are shared by every organisation that adds them.
- **Private repositories** are shared only between organisations that each proved access with their own token (a successful fetch of the same remote and commit). Otherwise each organisation gets its own graph.
- Organisation-owned data (reviews, memory, decisions, schedules, settings) is never shared.

## Authentication and authorisation

- **Sessions:** login sets an HTTP-only, `SameSite=Lax`, `Secure` (behind TLS) cookie holding a random token; only its hash is stored. Sessions expire after 14 days and on logout or password change.
- **API keys:** `Authorization: Bearer csk_…`. Every MCP connection, hook and CLI call uses one, so usage, memory and reviews are attributed to a user and organisation and never mix.
- **Service token:** the existing `CODESYNAPSE_API_TOKEN` keeps working for single-tenant installs and internal calls (MCP → API). It acts as a platform admin.
- **Without a database** (`CODESYNAPSE_DATABASE_URL` unset) the API runs single-tenant exactly as before.
- **Brute force:** failed logins are rate limited per email and per client address.
- **Audit log (NIS2):** every changing request (route, target, status, outcome) and every security event (sign-up, login success and failure, rate limiting, logout, password change, key creation and revocation, organisation and membership changes, refused requests) with actor, authentication method, key, organisation, client address, user agent and request ID. Entries are append-only and hash-chained (each hash covers the entry and the previous hash; `GET /api/v1/admin/audit/verify` checks the chain), exportable as JSON, JSON Lines or CSV (organisation admins: their organisation; platform admins: everything), written as structured `audit` log lines for SIEM collection, and kept `CODESYNAPSE_AUDIT_RETENTION_DAYS` (default 400).
- **Roles:** viewers read; members also run syncs, reviews and agents; admins manage repositories, settings and members; owners also manage billing-free organisation lifecycle (rename, delete, transfer).

## Product structure (web app, phase 3)

A calm product UI with light and dark themes, neutral surfaces, one brand accent and status colours only for meaning. Plain language, an explanation next to every figure, and costs in money where possible.

1. **Overview**: what needs attention now across the organisation (critical security findings, vulnerabilities you actually call, licence conflicts, stale repositories), and what agents saved.
2. **Repositories**: list with health at a glance; each repository has Summary, Security, Licences, Dependencies, Code health, Reviews and Memory.
3. **Agents**: connect an agent in three steps (create a key, copy the setup for Claude Code / Codex / Cursor, verify), then see sessions, tokens saved and memory.
4. **Settings**: organisation (members and roles, Git tokens per host, AI models, licence policy, sync defaults) and personal (profile, password, API keys, sessions).
5. **Admin** (platform admins): organisations, users, cross-tenant statistics, system health.
6. **Help**: getting started and "what does this mean" pages linked from every screen.

## Phases

1. **Accounts and audit** (done): PostgreSQL with embedded migrations; users, organisations, memberships, sessions, API keys; the hash-chained audit log with export and verification; signup (first user is platform admin), login, logout, `me`, organisations and members, personal API keys; auth middleware accepting session, API key or the legacy service token; single-tenant mode unchanged without a database.
2. **Tenant scoping**, in three parts:
   - **2a (done):** repository registry in PostgreSQL (`repositories`: organisation, graph, remote, visibility); every API route carries an authorisation policy; lists, overview, statistics and jobs show the organisation's repositories only; other organisations' repositories, reviews and jobs answer 404; an organisation adding an already indexed public repository is linked to the existing graph (de-duplication); organisations add public repositories only, cloned without the installation's tokens; deleting removes the link and the graph with the last link; platform admins attach graphs indexed before accounts (`POST /api/v1/accounts/orgs/{org}/repos`).
   - **2b (done):** the MCP server authenticates personal API keys (checked on every request, so revocation and removed memberships take effect at once) and scopes every tool to the key's organisation; agent memory is kept per organisation even on a shared graph (`org|graph` namespaces, also for the API's offload and recall); reviews carry their organisation; usage counters carry the organisation and the statistics show an organisation its own; calls the MCP server makes to the API on a user's behalf use that user's key.
   - **2c:** organisation settings: Git tokens per host, encrypted at rest, which also allow private repositories (shared only after the organisation proves access with its own token).
3. **New web app**: the product structure above, login and signup, settings, agent onboarding.
4. **Admin and statistics**: platform admin area, cross-tenant statistics, per-organisation statistics.
5. **Production on Kubernetes**: PostgreSQL (operator or managed), secrets, ingress with TLS, horizontal scaling of API and MCP, backups, upgrade notes.
6. **Single sign-on**: OpenID Connect login and just-in-time accounts.
