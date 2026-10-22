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
5. **Administration** (platform admins): system health, organisations, users (suspend, platform admin), cross-tenant statistics and the audit log.
6. **Help**: getting started and "what does this mean" pages linked from every screen.

## Phases

1. **Accounts and audit** (done): PostgreSQL with embedded migrations; users, organisations, memberships, sessions, API keys; the hash-chained audit log with export and verification; signup (first user is platform admin), login, logout, `me`, organisations and members, personal API keys; auth middleware accepting session, API key or the legacy service token; single-tenant mode unchanged without a database.
2. **Tenant scoping**, in three parts:
   - **2a (done):** repository registry in PostgreSQL (`repositories`: organisation, graph, remote, visibility); every API route carries an authorisation policy; lists, overview, statistics and jobs show the organisation's repositories only; other organisations' repositories, reviews and jobs answer 404; an organisation adding an already indexed public repository is linked to the existing graph (de-duplication); organisations add public repositories only, cloned without the installation's tokens; deleting removes the link and the graph with the last link; platform admins attach graphs indexed before accounts (`POST /api/v1/accounts/orgs/{org}/repos`).
   - **2b (done):** the MCP server authenticates personal API keys (checked on every request, so revocation and removed memberships take effect at once) and scopes every tool to the key's organisation; agent memory is kept per organisation even on a shared graph (`org|graph` namespaces, also for the API's offload and recall); reviews carry their organisation; usage counters carry the organisation and the statistics show an organisation its own; calls the MCP server makes to the API on a user's behalf use that user's key.
   - **2c (done):** organisation Git tokens per host (`PUT/DELETE /api/v1/accounts/orgs/{org}/git-tokens/{host}`, owners and admins), sealed with AES-256-GCM under `CODESYNAPSE_SECRET_KEY` and bound to organisation and host; members see host and preview. Organisations add private repositories after proving access with their own token (one `ls-remote`); the clone, syncs, branch lists and fix-branch pushes use it, never the installation's tokens. A graph is shared only between organisations that each proved access, and only for the same remote. Every sync re-checks each organisation's access: a refused or removed token hides the graph from that organisation (audited) until a working token restores it; network failures change nothing. Also fixed on the way: organisations could not add public repositories without naming a branch, and a public URL registered under a private graph's name would have linked that graph.
2d. **Relationship-based access (ReBAC)** (done): Zanzibar-style relation tuples in PostgreSQL with a Go engine (no extra service). Teams in organisations (members and maintainers, with an organisation role for the whole team); a guest role that inherits nothing; per-repository grants to people and teams (viewer, editor, admin); read-only sharing of one repository with a person or team in another organisation; reviews and agent memory follow the repository's grant. The API and MCP compute access the same way; agents of viewers and share recipients are read-only; every grant change is audited. UI: Settings → Teams, and an Access page on each repository.
3. **New web app**, in two parts:
   - **3a (done):** the calm design system (light and dark, one accent, status colours for meaning, validated chart palettes per theme) applied to every page through tokens; sign-in, sign-up and first-run setup; the web proxy forwards the user's session instead of the installation token (which made every visitor a platform admin) and reports the client address to the API through `CODESYNAPSE_TRUSTED_PROXIES`; organisation switcher; Settings (profile and password, API keys, members and roles, Git tokens); Agents (create a key, copy the setup for Claude Code, Codex, Cursor or any MCP client, see the first call arrive); Help (getting started and what every figure means).
   - **3b (done):** Overview is "what needs attention" (`GET /api/v1/attention`, also per repository with `?repo=`): called vulnerabilities, critical and high findings, licence conflicts, private repositories that lost access, failing syncs, other vulnerabilities, repositories no security rule covers, stale indexes and missing update automation, ranked and each with a plain explanation and a link to where it is fixed; repositories with nothing flagged are listed as all clear. Repository pages are grouped as Repository (Summary with its own attention list, Security, Dependencies, Licences, Code health = Quality + Unused code, Access), Work (Reviews, Agent memory, Impact of a change, AI assistant) and Explore (Graph, Symbols, Kubernetes & Helm, Docs, Packages). The new Dependencies page lists every package with version, licence verdict, vulnerabilities with reachability and fixed version, and the Renovate and Dependabot findings.
4. **Administration**, in three parts:
   - **4 (done):** the Administration area for platform admins. Overview: health of the graph store, accounts database, live statistics and audit chain, with timings; counts of people, organisations and repositories; repositories that belong to no organisation, each assignable in one step; tool calls by organisation, model cost and latency over 24 hours, 7 or 30 days. Organisations: members per role, repositories, repositories that lost access, teams. Users: memberships, active API keys, last agent use; suspend and resume accounts (a suspended account is signed out everywhere, cannot sign in and its keys stop working, while memberships and history stay) and grant or revoke platform admin, never leaving the installation without an active admin and never suspending yourself. Audit log: filters by kind of action, person, outcome and period, entry details, CSV and JSON Lines export of the filtered period, and chain verification. Every change is audited (`user.suspend`, `user.resume`, `user.platform_admin.grant|revoke`).
   - **4b, privacy and model cost (done):** personal data is masked in free text before it is stored or sent to a model (`redact.Personal`, and `redact.Free` for secrets plus personal data): payment card numbers (issuer prefix, length and Luhn), national identity numbers (US SSN with unissued ranges excluded; Nordic DDMMYY/YYMMDD/YYYYMMDD-NNNN with a real date, eight-digit dates not in the future) and phone numbers (international with +, North American, and eight digits in pairs), replaced by `[CARD]`, `[NATIONAL-ID]` and `[PHONE]` so the sentence still reads. It applies to agent memory names, offloaded turns, review text, every model prompt and every embedding input. Only formatted or validated numbers match; hashes, versions (including semver build metadata), line numbers, timestamps, UUIDs, IP addresses, dates and diff hunks are left alone (tested, and fuzzed for idempotence). Ingest summaries keep secret redaction only. Anthropic calls mark the system prompt (always a constant) as the cache prefix, so repeated calls of a task read it from the prompt cache once it is longer than the model's minimum; cached tokens count in `llm_tokens_saved_total` and in the cost. Nearest-neighbour memory search returns 3 to 10 hits (8 by default; was up to 50) in every store (`vector.ClampK`).
   - **4c, search (done):** `docs/adr/0001-search.md` compares search inside the stores we run (Milvus BM25 and dense vectors with RRF, HugeGraph and Neo4j indexes) with a separate engine fed by change data capture (Elasticsearch, which is not Apache-2.0 compatible, or OpenSearch/Solr, which are) on footprint, freshness, tenant isolation and revocation, latency, answer size and licences, and keeps search in the stores. Agent memory recall is now hybrid in the proper sense: a BM25 leg over the namespace's entity names, aliases and types (`MemoryLexical`, one ranker for all three stores, about 5 ms at the 5,000-entity cap) and the vector leg are fused by reciprocal rank, deduplicated and cut to 3–8 entries, each saying which legs found it; both legs read only the caller's namespace, and answers never carry vectors. Before, vector and substring scores were mixed as if on one scale. Code search keeps exact identifier lookup first and graph traversal for structure (source is not embedded). The ADR names when to move a leg into Milvus' native BM25 (longer text such as doc sections and reviews, p95 above 50 ms, larger memory caps) and the only conditions under which a separate engine would be worth it.
5. **Production on Kubernetes**: PostgreSQL (operator or managed), secrets, ingress with TLS, horizontal scaling of API and MCP, backups, upgrade notes.
6. **Single sign-on**: OpenID Connect login and just-in-time accounts.
