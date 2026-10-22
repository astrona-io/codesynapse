# Tenant access and platform administrator isolation

Status: implemented in SaaS phase 4d, extending phase 2d. The implementation removes platform-admin content bypasses and replaces strongest-path-wins with direct-user, team, then organisation-default precedence. The design examples below explain the contract; store adapters contain the executable queries.
Session-scoped tool execution and chat memory must also follow [phase 4g](SESSION_CONTEXT_PLAN.md): immutable runtime identity, inherited parent context and tenant/session/namespace filtering. This requirement does not turn session IDs into metric labels.

## Implementation and upgrade

- PostgreSQL migration `0006_access_policy.sql` adds organisation defaults, policy revisions, a unique repository-subject override and a durable projection outbox. Existing and new organisations default to `NONE`; the migration does not recreate old implicit grants. Contradictory legacy grants fail closed and require explicit repair. Existing external grants remain inactive without membership in the owning organisation.
- Before upgrading, identify the explicit owners who will restore intended repository permissions. After migration, an owner can request `GET /api/v1/accounts/orgs/{org}/access-changes` with their signed-in session to see inactive external grants and repositories lacking an active administrator. Use Settings → Organisation to assign an existing member as repository administrator, or call `POST /api/v1/accounts/orgs/{org}/repos/{repo}/recover-admin` with `{"user":"member@example.com"}`. Recovery and policy changes are audited. If legacy grants are malformed, recovery replaces them with explicit denies for affected members before granting the designated administrator; it does not restore broad inherited access. A platform administrator without organisation membership cannot perform recovery.
- Settings → Organisation offers `NONE`/`READ_ONLY` defaults. The repository Access page distinguishes direct grants from the default and supports a user-only no-access override. API roles remain `viewer`, `member`, `admin`, and `none`; these correspond to read, write, admin, and deny. Removing a direct grant restores inheritance.
- HTTP and stdio MCP in accounts mode require a personal API key; the installation MCP token is rejected. Single-tenant/offline mode remains available without the accounts database. Organisation-less legacy reviews are unavailable in accounts mode. Existing schedules without an actor/organisation delegation remain dormant until a repository administrator configures them again. Installation-wide forge scanning and mounted-source ingestion are unavailable in accounts mode; register repositories from a signed-in session with the organisation's own Git credentials. Personal API keys cannot register or attach repositories because creation records a new administrator grant.
- The API rebuilds dirty organisation projections immediately and every 30 seconds, with a 20-second batch deadline. Rebuilds publish complete, versioned access vertices outside the code graph. Failed writes leave the outbox entry pending, and acknowledgement cannot erase a newer change. All authorization continues through `accounts.AccessOf`/`ResolveRepository`, including while a graph backend is unavailable. The snapshots cap total items at 5,000 and teams per user at 128; an oversized projection remains pending and grants no authority.
- `store.ForRepository` confines contextual queries, direct node IDs, impact analysis and graph traversals to the authorized graph. Queued work checks its saved principal again before execution and before persistence or push; removing membership, revoking credentials or reducing permissions invalidates the work.

The reference resolver's benchmark with 128 teams and 100 repositories measured about 0.21 ms per evaluation with the memory store and 2.22 ms with disposable PostgreSQL 17 on an Apple M5 Pro. A contract test checks seven bulk reads independent of the team count. This is a local resolver measurement, not a database latency SLA.

## Permission rules

Platform administration and repository access are separate capabilities. A platform administrator can manage accounts and installation settings and see bounded operational telemetry. That role grants no repository listing, symbols, summaries, code traversal, reviews or memory. Selecting an organisation header must never manufacture membership. Service credentials have no implicit organisation membership either.

A platform administrator who is also an explicitly enrolled organisation member accesses code through the same checks as any other member. This describes the current implementation. The planned [bootstrap-root separation follow-up](ROOT_BOOTSTRAP_PLAN.md) introduces an immutable root account classification that denies all ordinary organisation work even with membership or repository grants; standard platform administrators remain a distinct identity category. Root must first provision an organisation and a standard owner, then sign out so the operator can authenticate as that standard user. Store platform status in the accounts database, not as a graph relationship to `Repository`, `Package` or `Function`. Do not create `SystemAdmin` or `RootAdmin` graph vertices or grant edges. Account administration must not offer an unaudited way to enrol yourself in an organisation: require the organisation's owner to approve new membership. Platform suspension remains available and immediately removes access.

For a usable repository entry in the authenticated organisation, resolve access in this order:

1. Require an active user, current key/session and explicit membership in that organisation. Team membership alone cannot bypass this gate; adding a team member requires organisation membership.
2. Use a direct user grant if present, including an explicit `NONE` override.
3. Otherwise use the strongest grant from the user's teams in that same organisation.
4. Otherwise apply `default_repo_access`: `READ_ONLY` or `NONE`. New organisations start at `NONE`. Guests receive no default access and need a grant.

This defines the previously unspecified conflict between a direct grant and a team grant: direct user grants take precedence. A direct `READ` can reduce a team's `ADMIN`; a direct `NONE` can block an organisation's read default. Within the team tier the strongest grant wins. `NONE` is an explicit deny extension for individual overrides; removing a grant restores inheritance. Organisation owner/admin roles manage organisation policy and membership but do not silently override a repository data permission.

| Effective permission | Allowed actions |
|---|---|
| `NONE` | No repository discovery or content |
| `READ` (`VIEW`, `READ_ONLY`, existing `viewer`) | Symbols, call graphs, minimal context, repository lists, review reading and memory recall |
| `WRITE` (existing `editor`) | Read plus review submission/decisions, memory upserts and offloading |
| `ADMIN` (existing repository `admin`) | Write plus repository sync, indexing schedules, repository settings and grants |

Repository creation requires an organisation capability and atomically records an explicit, audited initial `ADMIN` grant to the creating member (or a designated existing member). Reject creation without that grant. Prevent voluntary removal of the last active repository administrator. Security suspension, credential revocation and lost private-repository access must still take effect immediately, even if they leave no active repository administrator; the explicit owner-recovery path remains available. Recovery is an explicit, audited reassignment by an organisation owner to an existing member; it does not grant the owner code access automatically and platform administrators cannot perform it without the organisation role.

Keep organisation lifecycle, membership and Git-token administration as separate organisation capabilities. Audit every policy/grant change and denial. Return 404 for repositories outside the caller's visibility; return 403 for insufficient permission on an already visible repository. Unknown tools/actions fail closed. A review update must resolve its stored repository and organisation before checking permission.

Existing cross-organisation shares need migration: the recipient must first accept explicit guest membership in the owning organisation, then receive a repository grant there. Until that happens the share is inactive. A key for organisation A cannot use a grant in B. A shared code graph never combines permissions or private content from different organisation entries.

## Authoritative data and graph schema

PostgreSQL remains the authority for users, sessions, membership and relation tuples. Add an organisation default policy and an access-policy revision, plus a unique direct override per `(organisation, repository, user)`. Keep one grant per team/repository and enforce tenant ownership with foreign keys and service validation. Policy changes and their audit/outbox records commit together.

The graph schema below is a minimal, rebuildable access projection for parity checks and explaining access paths. It never contains passwords, API keys, email addresses or platform-admin grants. A graph answer alone cannot authorize a request: check the current accounts state on every request. If the projection is missing or stale, use the authoritative resolver or deny. Never reuse a stale positive result. This avoids delayed revocation across PostgreSQL and two graph backends.

`Repository` already means the shared indexed code root. Use `AccessRepository` for the logical organisation-owned Repository in this access model; it has `graph_id` pointing to the existing code graph by identifier, not an unrestricted traversal edge. Replacing the existing label would break deduplication and code queries.

| Logical entity | Properties and identity |
|---|---|
| `Organization` | `tenant_id` (unique), `default_repo_access`, `policy_revision` |
| `User` | `(tenant_id, user_id)`, `active`, `inherits_default`; tenant-local projection of an explicitly enrolled user |
| `Team` | `(tenant_id, team_id)` |
| `AccessRepository` (logical Repository) | `(tenant_id, repo_id)`, `graph_id`, `usable` |
| `MEMBER_OF` | User → Organization, User → Team; `tenant_id` |
| `BELONGS_TO` | Team → Organization, AccessRepository → Organization; `tenant_id` |
| `HAS_ACCESS` | User or Team → AccessRepository; `tenant_id`, `role`, `rank` (`NONE=0`, `READ=1`, `WRITE=2`, `ADMIN=3`) |

All endpoints of a relationship must have the same `tenant_id`. The writer validates role/rank consistency and cardinality; no endpoint accepts arbitrary labels or relationship names. Never attach access edges directly to packages or functions. Scope every subsequent code query to the resolved `graph_id` and every content query to the resolved `tenant_id` and namespace.

### Neo4j definition

Illustrative schema statements for the implementation, not an applied migration:

```cypher
CREATE CONSTRAINT access_org IF NOT EXISTS
FOR (n:Organization) REQUIRE n.tenant_id IS UNIQUE;
CREATE CONSTRAINT access_user IF NOT EXISTS
FOR (n:User) REQUIRE (n.tenant_id, n.user_id) IS UNIQUE;
CREATE CONSTRAINT access_team IF NOT EXISTS
FOR (n:Team) REQUIRE (n.tenant_id, n.team_id) IS UNIQUE;
CREATE CONSTRAINT access_repo IF NOT EXISTS
FOR (n:AccessRepository) REQUIRE (n.tenant_id, n.repo_id) IS UNIQUE;
```

Uniqueness constraints do not enforce property presence, allowed role values or edge cardinality; validate those in the writer and contract tests. See the [Neo4j constraint reference](https://neo4j.com/docs/cypher-manual/current/schema/constraints/create-constraints/).

### HugeGraph definition

Register `tenant_id`, `user_id`, `team_id`, `repo_id`, `graph_id`, `default_repo_access` and `role` as text properties; `active`, `inherits_default` and `usable` as booleans; `rank` as integer; `policy_revision` as long. Define the four vertex labels with the compound identities above, using the existing deterministic ID scheme and indexed tenant/identity lookups.

HugeGraph edge labels have fixed source/target labels, so the adapter maps logical relationships to these fixed physical edge labels:

| Logical relationship | HugeGraph label | Source → target |
|---|---|---|
| `MEMBER_OF` | `USER_MEMBER_OF_ORG` | User → Organization |
| `MEMBER_OF` | `USER_MEMBER_OF_TEAM` | User → Team |
| `BELONGS_TO` | `TEAM_BELONGS_TO_ORG` | Team → Organization |
| `BELONGS_TO` | `REPO_BELONGS_TO_ORG` | AccessRepository → Organization |
| `HAS_ACCESS` | `USER_HAS_ACCESS` | User → AccessRepository |
| `HAS_ACCESS` | `TEAM_HAS_ACCESS` | Team → AccessRepository |

Define these as single edges per endpoint pair, with required `tenant_id`, and `role`/`rank` on access edges. Provision through constant schema definitions in `hugegraphstore/schema.go`, not model-generated scripts. Add the logical/physical mappings to the model allowlists, `nodeprops`, schema coverage tests and the shared store contract. The [HugeGraph schema API](https://hugegraph.apache.org/docs/clients/restful-api/schema/) describes property keys, vertex labels, edge labels and indexes; validate the migration against the pinned 1.5 image.

## Bounded query evaluation

`internal/accounts` owns the authoritative permission resolver. `internal/query` can compose its explanation with the graph projection through a narrow store method. Store adapters own the following fixed query templates. Tenant, subject, repository and current policy revision come from authenticated context and server-side repository resolution; all are parameters/bindings. Model arguments are selectors to validate, never authority.

The examples return a numeric rank. Missing rows/vertices, duplicate direct grants, invalid ranks, stale revisions and backend failures do not mean access. Bound request time, membership count and result size; reject an over-limit membership set rather than partially evaluating it. No variable-depth recursion or full code-graph scans are needed.

### Cypher candidate

```cypher
MATCH (o:Organization {tenant_id: $tenant_id, policy_revision: $policy_revision})
MATCH (u:User {tenant_id: $tenant_id, user_id: $user_id, active: true})
      -[:MEMBER_OF {tenant_id: $tenant_id}]->(o)
MATCH (r:AccessRepository {tenant_id: $tenant_id, repo_id: $repo_id, usable: true})
      -[:BELONGS_TO {tenant_id: $tenant_id}]->(o)
OPTIONAL MATCH (u)-[d:HAS_ACCESS {tenant_id: $tenant_id}]->(r)
WITH o, u, r, collect(d) AS direct
WHERE size(direct) <= 1 AND all(x IN direct WHERE
  x.rank IN [0, 1, 2, 3] AND x.rank = CASE x.role
    WHEN 'NONE' THEN 0 WHEN 'READ' THEN 1 WHEN 'WRITE' THEN 2
    WHEN 'ADMIN' THEN 3 ELSE -1 END)
CALL {
  WITH u, o
  MATCH (u)-[:MEMBER_OF {tenant_id: $tenant_id}]->
    (t:Team {tenant_id: $tenant_id})-[:BELONGS_TO {tenant_id: $tenant_id}]->(o)
  WITH DISTINCT t LIMIT $team_probe_limit
  RETURN collect(t) AS teams
}
WITH o, u, r, direct, teams WHERE size(teams) <= $max_teams
CALL {
  WITH teams, r
  UNWIND teams AS t
  OPTIONAL MATCH (t)-[g:HAS_ACCESS {tenant_id: $tenant_id}]->(r)
  WITH t, collect(g) AS grants
  RETURN collect({grants: grants}) AS team_grants
}
WITH o, u, r, direct, team_grants
WHERE all(tg IN team_grants WHERE size(tg.grants) <= 1 AND
  all(x IN tg.grants WHERE x.rank IN [1, 2, 3] AND x.rank = CASE x.role
    WHEN 'READ' THEN 1 WHEN 'WRITE' THEN 2 WHEN 'ADMIN' THEN 3 ELSE -1 END))
WITH o, u, r, direct,
  reduce(rs = [], tg IN team_grants | rs + [g IN tg.grants | g.rank]) AS ranks
RETURN r.graph_id AS graph_id,
  CASE WHEN size(direct) = 1 THEN direct[0].rank
       WHEN size(ranks) > 0 THEN reduce(best = 0, x IN ranks | CASE WHEN x > best THEN x ELSE best END)
       WHEN u.inherits_default AND o.default_repo_access = 'READ_ONLY' THEN 1
       ELSE 0 END AS permission
```

### Gremlin candidate

The following is a constant script; identifiers below are binding variables. This is a draft to validate on the pinned backend before implementation. The fixed physical edge labels are the mapping above.

```groovy
def orgs = g.V().hasLabel('Organization').has('tenant_id', tenant_id)
  .has('policy_revision', policy_revision).limit(2).toList()
if (orgs.size() != 1) return [permission: 0]
def org = orgs[0]
def users = g.V().hasLabel('User').has('tenant_id', tenant_id)
  .has('user_id', user_id).has('active', true)
  .where(__.outE('USER_MEMBER_OF_ORG').has('tenant_id', tenant_id)
    .inV().hasId(org.id())).limit(2).toList()
def repos = g.V().hasLabel('AccessRepository').has('tenant_id', tenant_id)
  .has('repo_id', repo_id).has('usable', true)
  .where(__.outE('REPO_BELONGS_TO_ORG').has('tenant_id', tenant_id)
    .inV().hasId(org.id())).limit(2).toList()
if (users.size() != 1 || repos.size() != 1) return [permission: 0]
def user = users[0]
def repo = repos[0]
def direct = g.V(user.id()).outE('USER_HAS_ACCESS').has('tenant_id', tenant_id)
  .where(__.inV().hasId(repo.id())).limit(2).toList()
def validGrant = { edge, allowNone ->
  if (!edge.property('role').isPresent() || !edge.property('rank').isPresent()) return false
  def expected = ['NONE': 0, 'READ': 1, 'WRITE': 2, 'ADMIN': 3][edge.value('role')]
  return expected != null && (allowNone || expected > 0) && edge.value('rank') == expected
}
if (direct.size() > 1 || direct.any { !validGrant(it, true) }) return [permission: 0]
def teams = g.V(user.id()).outE('USER_MEMBER_OF_TEAM').has('tenant_id', tenant_id)
  .inV().hasLabel('Team').has('tenant_id', tenant_id)
  .where(__.outE('TEAM_BELONGS_TO_ORG').has('tenant_id', tenant_id)
    .inV().hasId(org.id())).dedup().limit(max_teams + 1).toList()
if (teams.size() > max_teams) return [permission: 0]
def ranks = []
for (team in teams) {
  def grants = g.V(team.id()).outE('TEAM_HAS_ACCESS').has('tenant_id', tenant_id)
    .where(__.inV().hasId(repo.id())).limit(2).toList()
  if (grants.size() > 1 || grants.any { !validGrant(it, false) }) return [permission: 0]
  ranks.addAll(grants.collect { it.value('rank') })
}
if (direct.size() == 1) return [graph_id: repo.value('graph_id'), permission: direct[0].value('rank')]
def fallback = user.value('inherits_default') && org.value('default_repo_access') == 'READ_ONLY' ? 1 : 0
return [graph_id: repo.value('graph_id'), permission: ranks.isEmpty() ? fallback : ranks.max()]
```

Use `max_teams` from a server constant and compute `team_probe_limit = max_teams + 1` in the adapter, never from the client. Count edges independently of their properties so a missing rank cannot fall through to broader inherited access. Validate duplicate/invalid grants and test the same limits in Cypher, Gremlin and the Go resolver. Profile both backends; if per-team traversal cost is excessive, use one bounded bulk traversal rather than increasing the limit. The authoritative resolver remains the request gate while these templates are validated.

## Go and MCP enforcement

The current bearer keys are opaque `csk_…` values, not JWTs. Keep their hashed lookup and per-request revocation checks. For future OIDC, verify signature, issuer, audience, expiry and accepted algorithm before mapping `sub` to an existing local user; an unverified JWT payload supplies no identity or role. Organisation membership is always loaded locally.

The implementation flow is:

```text
authenticate request -> local Principal{UserID, KeyID, OrgID, Via}
require explicit organisation membership for content operations
resolve repository selector within that organisation -> RepositoryEntry
resolve authoritative permission for (UserID, OrgID, RepositoryEntry.ID)
look up required permission from a fixed action/tool policy table
if insufficient: audit denial and return 404/403
put AuthorizedRepository{TenantID, GraphID, ContentNamespace, Permission, Revision} in context
run the handler using that scope for every graph/vector/content lookup
```

Illustrative Go boundary (proposed APIs, not existing compiled code):

```go
var toolPermissions = map[string]Permission{
    "get_symbol_context": Read, "get_call_graph": Read, "minimal_context": Read,
    "submit_review": Write, "update_review_comment": Write,
    "upsert_graph_triples": Write, "offload_turns": Write,
}

func (a *Authorizer) AuthorizeTool(ctx context.Context, tool, selector string) (context.Context, ScopedReader, error) {
    execution, ok := ExecutionFrom(ctx) // validated by ingress, never tool arguments
    if !ok || execution.SessionID == "" || execution.ParentSessionID != execution.SessionID {
        return ctx, nil, ErrMissingSession
    }
    principal, ok := PrincipalFrom(ctx)
    if !ok || principal.UserID == "" || principal.OrgID != execution.TenantID {
        return ctx, nil, ErrUnauthenticated
    }
    required, known := toolPermissions[tool]
    if !known {
        return ctx, nil, ErrUnknownTool
    }
    // Resolver checks current membership, session validity and repository policy.
    repo, err := a.ResolveRepository(ctx, principal, selector, required)
    if err != nil {
        a.AuditDenied(ctx, tool, err)
        return ctx, nil, err
    }
    scoped := a.Store.ForAuthorizedRepository(repo, execution)
    return WithAuthorizedRepository(ctx, repo), scoped, nil
}
```

The table is an excerpt; enumerate all served tools before rollout and test coverage. The handler runs only with the returned context/scoped reader. Read-only annotations do not replace authorization. API sync, schedule and grant endpoints use the same resolver with `Admin`; this plan does not introduce new MCP write tools for those actions.

Add a typed Go resolver method such as `ResolveRepository(ctx, principal, selector, action) (AuthorizedRepository, error)`. Keep its scope fields private or constructed only by the resolver. `internal/agentctx` receives an authorized, scoped reader; it does not accept a user-supplied tenant. API policies and MCP policy tables call the same resolver. Resolve batch/list operations against the same scope and never expose unfiltered ambiguity suggestions, counts, caller graphs or impact results.

Remove the `scope{all:true}` platform shortcut and synthetic owner membership from `services/api/internal/api/scope.go`. In `services/mcp/internal/mcpserver/scope.go`, an absent `Caller` must deny content in accounts mode instead of selecting all graphs. Make service principals explicit and separate platform-control endpoints from repository endpoints. Forward the caller's own key for offload/review work; background jobs carry an organisation/repository identity and re-check permission before execution and storage.

For HTTP MCP, run authentication on every request and authorization on every tool invocation, including resumed sessions. For stdio, retain explicit offline/local single-user mode. A stdio process connected to a multi-tenant deployment must authenticate through a personal key from the environment/config token store, or refuse startup; being a local process must not confer platform access. Never accept a token in tool arguments.

## Delivery and acceptance

1. Add policy/override migrations, membership invariants and the reference resolver with table-driven tests. Produce an access-change report for existing organisations. Do not grant broad access to preserve old platform bypasses. Existing external shares remain denied until accepted guest membership is recorded.
2. Remove API/MCP bypasses, classify every action, and apply the resolver to reads, mutations, jobs and contextual tools. Update `docs/AGENT_GUIDE.md` and `AGENTS.md` with the new rules in that implementation change.
3. Add the versioned access projection and store contract, including rebuild/outbox recovery. Use current PostgreSQL permissions for enforcement even during rebuild; compare Neo4j, HugeGraph and memory answers on the same fixtures.
4. Update organisation defaults and repository Access UI with inherited versus direct permissions and an explicit deny action. Keep platform dashboards free of code-derived content until membership authorizes it.
5. Run `just test`, `just lint`, `just web-check`, PostgreSQL contracts and both graph integration suites. Exercise revocation during a live MCP session and a queued background job before enabling production accounts mode.

Required cases: platform admin with no membership; platform admin with only read membership; spoofed tenant/header/repo; service-token and stdio bypass attempts; direct read over team admin; direct deny over org read; team grant over org none; two conflicting teams; guest without grant; disabled/deleted user; revoked key; removed team membership; lost private-Git access; same graph linked by two tenants; missing/stale projection; projection outage; over-limit teams; review/memory access through a foreign namespace; write-vs-admin sync boundaries. Measure indexed lookup latency and query count with a large membership set, and fail closed on partial results.
