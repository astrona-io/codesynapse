# OpenFGA authorisation plan

Status: phase 4 done (2026-11-02): the model, the client, the compose service and the contract run; nothing asks OpenFGA for decisions yet.

Repository and organisation access moves from the Go evaluator in `internal/accounts/authz.go` to [OpenFGA](https://openfga.dev) (Apache-2.0, a CNCF project). At the same time, permissions get one naming scheme, `resource:level`, where the level is `read`, `write` or `full`, and they become fine-grained: `resource.area:level` permissions that organisations can bundle into their own roles.

Decisions taken with the project owner:

- **Direct cutover.** OpenFGA makes the decisions from the release that turns it on. There is no shadow period. The safety net is a contract test that runs every existing access rule against OpenFGA before cutover (phase 4).
- **No bundled identity provider.** Sign-in stays as it is: built-in accounts plus one OpenID Connect provider per organisation (`docs/plans/SSO_PLAN.md`). Keycloak is supported as one of those providers, like Entra ID, Okta or Google. No service sits between the identity provider and OpenFGA. The API maps memberships, teams and grants to tuples itself.
- **PostgreSQL stays the record.** Memberships, grants, policy revisions, the audit chain and the outbox stay in the accounts database. OpenFGA holds a copy of the tuples, and that copy can always be rebuilt from PostgreSQL.

## Who does what

```
identity provider (any OIDC)  who you are, which groups you are in
CodeSynapse API               sign-in, sessions, API keys, memberships,
  + PostgreSQL                grants, audit and outbox (the record)
                              writes tuples to OpenFGA through the outbox
OpenFGA                       answers "may user U do X on Y?" and
                              "which repositories may U read?"
```

Some checks stay in Go before OpenFGA is asked, because they are about the account, not about relations:

- suspended accounts (`ErrSuspended`);
- root's governance-only rule (`businessAllowed`);
- API keys staying in their organisation;
- runtime session identity.

None of these become tuples.

## Permission names

Every permission is written `resource:level`. The levels are ordered: `full` includes `write`, and `write` includes `read`. `deny` is an explicit deny on a repository, not a level.

### Repository

| Today | New | What it allows |
|---|---|---|
| `viewer` | `repository:read` | Read the graph, reviews, memory, findings and reports; agents are read-only |
| `editor` | `repository:write` | Also write reviews, memory and catalog records; upload runtime facts and SARIF |
| `admin` | `repository:full` | Also sync, schedules, settings, grants and deletion |
| `none` | `repository:deny` | Explicit deny: overrides team grants and the organisation default |

### Organisation

| Today | New | What it allows |
|---|---|---|
| `guest` | `organisation:read` + guest | Sees the organisation, gets no repository default |
| `viewer` | `organisation:read` | Sees the organisation and receives the repository default |
| `member` | `organisation:write` | Also adds personal repositories, watches, imports and the member features |
| `admin` | `organisation:full` | Also manages members, teams, policy, Git tokens, SSO and the prompt log |
| `owner` | `organisation:full` + owner | Also counts as an owner: the last owner cannot be removed, and owners can recover repository administration |

Two of today's roles do not fit three levels. Each keeps a marker beside its level instead of becoming a fourth level:

- `guest` is a property of the membership: it is `organisation:read` without the repository default.
- `owner` is a relation of its own (`organisation#owner`): it is `organisation:full` plus the owner rules.

The UI shows them as "Read (guest)" and "Full (owner)".

### Team

| Today | New |
|---|---|
| `member` | `team:read` (belongs to the team and receives its grants) |
| `maintainer` | `team:full` (also manages the team's members) |

A team's grant on the organisation or on a repository uses the same names, for example `team:payments#member` → `repository:write`.

## Fine-grained permissions

The three levels above become roles: named bundles of finer permissions. Each permission covers one area of a repository or an organisation and is written `resource.area:level`. Routes, MCP tools and the UI check a permission, never a role. That lets an organisation build its own roles, for example "security reviewer" or "CI uploader", without new code.

A level includes the levels below it within the same area. `repository.reviews:full` includes `repository.reviews:write` and `repository.reviews:read`. Areas never include each other.

### Repository permissions

| Permission | What it allows (routes and MCP tools that check it) |
|---|---|
| `repository.code:read` | The code graph: search, symbols, outline, call graphs, metrics, hotspots, history, cleanup, duplicates, dependencies, licences, SBOM, impact, ingestion reports, the brief plan and the read-only MCP code tools, including `ask_local` and `local_draft` |
| `repository.security:read` | Security report, findings, SARIF and image-scan findings, triage list, `security_report`, `attack_surface`, `security_symbols`, `license_report`, `rule_guide` |
| `repository.security:write` | Upload and delete SARIF and image scans |
| `repository.security:full` | Triage and suppress findings |
| `repository.reviews:read` | Reviews and their code, `list_reviews`, `get_review` |
| `repository.reviews:write` | Submit reviews and decide on comments (`submit_review`, `update_review_comment`) |
| `repository.reviews:full` | Delete reviews |
| `repository.fixes:write` | Create fix branches from accepted comments (pushes a `codesynapse/*` branch) |
| `repository.memory:read` | Recall agent and session memory |
| `repository.memory:write` | Save, offload, index and promote memory (`upsert_graph_triples`, `offload_turns`) |
| `repository.memory:full` | Clear the repository's memory and session memory (writers can do this today, so `repository:write` holds it) |
| `repository.architecture:read` | Records, proposals, links, code records, identities and identity reviews |
| `repository.architecture:write` | Propose, link, withdraw, annotate and record reuse (`architecture_propose`, `architecture_link`, `code_annotate`, `code_record_reuse`) |
| `repository.architecture:full` | Identity reviews (same symbol, copy, distinct) |
| `repository.runtime:read` | Coverage and test reports |
| `repository.runtime:write` | Upload and delete coverage and test reports |
| `repository.ai:write` | Model work for the repository through the API: AI tasks, the ask-local, local-draft and source-extract endpoints the bridge calls |
| `repository.settings:read` | Repository policy (`.codesynapse.yaml` approval), schedule, branches |
| `repository.settings:full` | Change the policy, schedule and remote |
| `repository.sync:write` | Start a sync or re-index |
| `repository.access:read` | See who has access and why |
| `repository.access:full` | Grant, change and remove access, recover administration |
| `repository.entry:full` | Delete the repository entry |

### Organisation permissions

| Permission | What it allows |
|---|---|
| `organisation.members:read` | List members |
| `organisation.members:write` | Invite people, list and cancel invitations |
| `organisation.members:full` | Change roles, remove members |
| `organisation.teams:read` | List teams and their members |
| `organisation.teams:full` | Create and delete teams, set team roles and members |
| `organisation.repositories:read` | List the organisation's repositories |
| `organisation.repositories:write` | Add repositories to your own space, list a forge's repositories for import, see the member-repositories setting |
| `organisation.repositories:full` | Add to the organisation space, set the repository default and the member-repositories setting |
| `organisation.watches:read` | List watches |
| `organisation.watches:write` | Create, change, pause and scan your own watches |
| `organisation.watches:full` | Change anyone's watch; include private repositories |
| `organisation.git_tokens:read` | See token previews (members can today, so `organisation:write` holds it) |
| `organisation.git_tokens:full` | Set and remove tokens; list private repositories with them |
| `organisation.sso:read` | See the provider settings |
| `organisation.sso:full` | Set and remove the provider and its group mappings |
| `organisation.gateway:read` | See gateway and prompt-log settings (members can today, so `organisation:write` holds it) |
| `organisation.gateway:write` | Send model traffic through the gateway |
| `organisation.gateway:full` | Change gateway settings |
| `organisation.prompt_log:read` | Read and export the prompt log |
| `organisation.prompt_log:full` | Turn the prompt log on or off, set its retention |
| `organisation.audit:read` | The organisation's audit log and everyone's agent sessions |
| `organisation.agent_sessions:read` | Your own agent sessions |
| `organisation.sessions:read` | Read a runtime session's activity |
| `organisation.sessions:write` | Register, report on and end your own runtime sessions (guests can: agents run as them) |
| `organisation#member` | Any membership, for routes the service checks further itself, such as a team maintainer changing their team's members |
| `organisation#owner` | The owner rules: attach an existing graph to the organisation, see access changes, recover repository administration |
| `organisation.roles:read` | List custom roles |
| `organisation.roles:full` | Create, change and delete custom roles |

Team permissions stay as in the table above (`team:read`, `team:full`). A team maintainer has `team:full` on that team only, which is the same as `organisation.teams:full` limited to one team.

### Built-in roles

The built-in roles give exactly the access people have today. Phase 2 proves that with `accountstest`, which checks every route and MCP tool for every role before and after.

| Role | Permissions |
|---|---|
| `repository:read` | Every repository `:read` permission, except `repository.access:read` |
| `repository:write` | `repository:read` + `security:write`, `reviews:write`, `fixes:write`, `memory:write`, `memory:full`, `architecture:write`, `runtime:write`, `ai:write` |
| `repository:full` | Every repository permission |
| `organisation:read` | `members:read`, `teams:read`, `repositories:read`, `watches:read`, `roles:read`, `sessions:read`, `sessions:write` |
| `organisation:write` | `organisation:read` + `repositories:write`, `watches:write`, `gateway:read`, `gateway:write`, `git_tokens:read`, `agent_sessions:read` |
| `organisation:full` | Every organisation permission |

The guest and owner markers keep their meaning. A guest gets `organisation#member`, `sessions:read` and `sessions:write` only: today a guest cannot list members, teams or repositories, and gets no repository default. An owner gets `organisation:full` plus the owner rules.

Which routes need which permission is fixed in Go: each route's policy names its permission (`repoPerm`, `reviewPerm`, `orgPerm` in `services/api/internal/api/scope.go`), `toolPolicies` does the same for MCP tools, and the floors live in `internal/accounts/permissions.go`. A test fails when a route or tool is missing from it, like `TestEveryRouteIsClassified` today. If phase 2 finds a route whose current rule does not fit the table, the table is changed to keep today's behaviour, and the plan is updated to say so.

### Custom roles

An organisation can define its own roles from these permissions:

- **Who:** only people with `organisation.roles:full`, in a signed-in session and never with an API key. Every change is audited before it is written.
- **No escalation:** you can only create or assign a role whose permissions you hold yourself on that scope. A custom role cannot contain the owner rules.
- **Scope:** a role holds either repository permissions or organisation permissions, never both. It is granted to people or teams on one repository, or on the whole organisation. An organisation-wide grant of repository permissions works like the repository default: it never applies to personal repositories.
- **Precedence:** unchanged. A direct grant on a repository, built-in or custom, replaces team grants and the default, and `repository:deny` still denies.
- **Limits:**
  - at most 50 custom roles per organisation;
  - at most 64 characters per role name;
  - names must not clash with a built-in role.
- **Change and removal:** changing a role's permissions changes them for everyone who holds it, so it bumps the organisation's policy revision. A role that is still granted cannot be deleted until its grants are removed.
- **Visible:** the Access page shows every permission a person has and the grant it came from.

Fine-grained permissions also apply to API keys: a key can be created with a subset of its owner's permissions, for example a CI key with only `repository.security:write` and `repository.runtime:write`. A key never has more than its owner has now. Losing a permission takes it away from the key at the next request.

### Where the names change

The rename and the permission table are phase 2, before OpenFGA, so OpenFGA starts with the final names.

- **Go:** the role constants in `internal/accounts` (`Role`, `Repo*`, `repoRelationRole`), route policies (`repoRole(...)`, `orgRole(...)`) and MCP tool policies.
- **PostgreSQL:** a migration renames tuple relations and membership roles. It keeps the guest and owner markers, and it does not bump policy revisions, because access stays the same.
- **API:** responses carry the new names only. Requests accept the old names during one release, map them to the new ones and log a deprecation.
- **SSO:** group mappings stored with old role names are migrated too.
- **Web:** `services/web/app/utils/types.ts`, the Settings pages (organisation, teams, SSO) and each repository's Access page.
- **Graph projections:** the access projections in `internal/model/access.go` and `internal/query/access.go`.
- **Docs:** `operations/accounts-access.md`, `docs/SAAS.md` 2d, `docs/plans/ACCESS_CONTROL_PLAN.md`, `AGENTS.md` and `docs/AGENT_GUIDE.md`.

## Authorisation model

This is the first draft in OpenFGA's DSL. Phase 4 put its core (users, organisations, teams and the three repository levels) in `internal/authz/fga/model.fga`, with a contract test for each row of the rules table below; the per-permission relations wait for the permission table of phase 2.

```
model
  schema 1.1

type user

type organisation
  relations
    define owner: [user]
    define full: [user, team#member] or owner
    define write: [user, team#member] or full
    define read: [user, team#member] or write
    define guest: [user]
    # Active non-guest members; written only for explicit memberships.
    define member: [user]
    define active: member or guest
    # Written as organisation:O#default_readers@organisation:O#member
    # only while the organisation's default is READ_ONLY.
    define default_readers: [organisation#member]

type team
  relations
    define organisation: [organisation]
    define full: [user]
    define member: [user] or full

type repository
  relations
    define organisation: [organisation]
    # Set only for organisation-space entries, never personal ones.
    define org_space: [organisation]
    define direct_full: [user]
    define direct_write: [user]
    define direct_read: [user]
    define direct_deny: [user]
    define has_direct: direct_full or direct_write or direct_read or direct_deny
    define team_full: [team#member]
    define team_write: [team#member]
    define team_read: [team#member]
    define default_read: default_readers from org_space
    # A direct grant replaces team grants and the default, even when weaker.
    define full: (direct_full or (team_full but not has_direct)) and active from organisation
    define write: (full or direct_write or (team_write but not has_direct)) and active from organisation
    define read: (write or direct_read or ((team_read or default_read) but not has_direct)) and active from organisation

    # Custom roles: a grant of custom role C to subject S on this repository
    # writes one tuple per permission in C, in the user_ or team_ tier.
    define user_reviews_write: [user]
    define team_reviews_write: [team#member]
    # One pair like this per permission. The built-in levels feed in through
    # the generated rule, so `write` holds reviews_write as it does today.
    define reviews_write: (write or user_reviews_write or (team_reviews_write but not has_direct)) and active from organisation
    define reviews_read: (reviews_write or read) and active from organisation
```

Phase 4 generates the per-permission relations from the Go permission table, so the model and the table cannot drift apart. A custom-role grant counts as a direct grant, so `has_direct` includes its `user_*` tuples.

### Rules the model must keep

These are today's rules. Every row becomes a test case in `accountstest` that runs against both the Go reference and OpenFGA.

| Rule | How the model keeps it |
|---|---|
| Only explicit, active members of the owning organisation get access | `and active from organisation` on every repository level; tuples are removed when a membership ends or the account is suspended |
| A direct user grant overrides team grants, even when weaker | `but not has_direct` on the team and default paths |
| Explicit `repository:deny` denies | `direct_deny` is part of `has_direct` and grants nothing |
| The strongest team grant in the same organisation wins | Union of `team_*`; the API writes team grants only for teams of the repository's organisation |
| Organisation default `READ_ONLY` gives `repository:read`; `NONE` gives nothing | The `default_readers` tuple exists only while the default is `READ_ONLY` |
| Guests get no default | Guests are in `guest`, not in `member`, so `default_readers` does not include them |
| Personal repositories get no default | Personal entries have no `org_space` tuple |
| Organisation roles give no code access | No organisation level appears in a repository relation |
| Platform admins and root get no code access | Never written as tuples; root is also refused in Go first |
| Lost or unusable entries are invisible | Their tuples are removed when the entry is marked `lost` and written again when it is restored |
| Custom roles never exceed their creator | Checked in Go when a role is created, changed or granted, before its tuples are written |
| A permission implies only its own area's lower levels | Generated relations chain only within an area |
| Overflow or invalid data fails closed | `ListObjects` results above `MaxTuples` and any OpenFGA error refuse the request |

## Consistency and failing closed

OpenFGA cannot join a PostgreSQL transaction, so writes go through the outbox:

1. A policy change commits in one transaction: the change, its audit entry, a higher policy revision and an outbox row with the tuple writes and deletes.
2. A bounded worker (the existing outbox worker, under the `api-loops` lease) sends the tuples to OpenFGA in order and records the last revision synced for each organisation.
3. A check for an organisation whose synced revision is behind its policy revision is refused, the same way a stale projection is refused today. A revoke takes effect when it commits, not when OpenFGA catches up: until the outbox is synced, the stale check fails closed.
4. Checks use `HIGHER_CONSISTENCY`. Any OpenFGA error, timeout or truncated answer refuses the request. There is never a fallback to "allow".

`codesynapse-api fga rebuild` writes every tuple again from PostgreSQL, and `fga verify` compares the two stores. Both are audited.

## Running OpenFGA

- **Compose:** an `openfga` service with its own database and user in the existing PostgreSQL. Migrations run as a separate init step. It is reachable on the internal network only, never published to the host.
- **Authentication:** a pre-shared key per service from the environment (`CODESYNAPSE_OPENFGA_TOKEN`) for the API, the workers and the MCP server, which computes access itself today. The playground and the unused port are switched off. Only the API writes tuples. OpenFGA's pre-shared keys are not limited to reads, so the MCP server's key could write too. See the open questions.
- **Store and model IDs:** created by the API at start-up (like `EnsureSchema`), recorded in PostgreSQL, and checked by every other process.
- **Telemetry:** check latency, errors and outbox lag are `platform_*` metrics, with no tenant, user or object labels. Spans carry the relation and object type only, never IDs.
- **Helm:** an `openfga` subchart, on by default and with its own `values.schema.json`, like the other stores. Per AGENTS.md, this waits for the final deployment phase. Until then, the chart keeps the Go evaluator, and the switch below stays off there.
- **Switch:** `CODESYNAPSE_AUTHZ=openfga` turns OpenFGA on. Without the switch, the Go evaluator keeps deciding until phase 6 removes it from production code. It stays as the test reference.

New dependencies, all Apache-2.0: the `github.com/openfga/go-sdk` Go module and the `openfga/openfga` image (pinned by digest). The test container uses the same image.

### Phase 4 as built

- **Model format:** `internal/authz/fga/model.fga` is the source people read and edit; `model.json` beside it is the same model in OpenFGA's JSON form, made once with OpenFGA's own transformer (`fga model transform`) and embedded in the binary. No DSL parser ships: `github.com/openfga/language` stays out. `TestModelDSLMatchesJSON` renders the JSON back to DSL and compares every relation, so neither file changes alone. When phase 2's permission table lands, a generator writes both files and the test keeps them honest.
- **Client** (`internal/authz/fga`): configured from `CODESYNAPSE_OPENFGA_URL` and `CODESYNAPSE_OPENFGA_TOKEN` only; https anywhere, http only to a single-label or cluster-local name (the rule of `telemetry.CheckAlertsURL`); no proxy, no redirects, a 2 s timeout on every call, one retry. `Check` and `BatchCheck` (at most 50) ask for `HIGHER_CONSISTENCY`; any error, missing answer or per-item error makes every answer false. `ListObjects` refuses an answer of `ListLimit` (1000, OpenFGA's own cap) or more as truncated. `WriteTuples`/`DeleteTuples` send at most 100 tuples in one transaction and ignore duplicates and missing deletes, so a replayed outbox batch is harmless. `EnsureStore`/`EnsureModel` find or create the store by name and write the model only when the stored one differs. The SDK's own telemetry is off: its attributes carry store and model IDs.
- **Contract:** `just test-openfga` starts a throwaway OpenFGA (in-memory datastore, a random pre-shared key) and runs `internal/authz/fga/contract_test.go`: every row of the table above that the model decides, plus idempotent writes and `EnsureModel` reusing the stored model. It lives beside the client rather than in `accountstest`, which phase 2 is reshaping; phase 6 runs the same rows against the Go reference there.
- **Compose:** `openfga-db-init` (the postgres image's `psql`) creates the `openfga` user and database idempotently and closes the database to everyone else; `openfga-migrate` runs OpenFGA's migrations; `openfga` runs with pre-shared keys, playground, metrics and profiler off, gRPC bound to 127.0.0.1 (the HTTP API reaches it in-process; the health check uses it), no published port. `just init` adds `OPENFGA_DB_PASSWORD` and `OPENFGA_API_KEY` (the one key OpenFGA accepts; see the open questions) to `.env`. Nothing reads them in the API, workers or MCP server yet.
- **Deviations:** `OPENFGA_LIST_OBJECTS_DEADLINE` is 5 s, above the client's 2 s timeout, so OpenFGA never cuts a list short and returns it as complete (it does so silently at its deadline); OpenFGA refuses a deadline above its 6 s request timeout. The table's "above `MaxTuples`" is `ListLimit`. One key instead of one per service, following the recommendation below.

### What phase 2 changed in the table

Phase 2 found these routes whose current rule did not fit the first draft of the table. Each keeps today's behaviour, and the tables above say so:

- guests hold no `organisation:read` permission, only membership and their own runtime sessions;
- `sessions:read`, `sessions:write`, `agent_sessions:read` and the `organisation#member` marker were added;
- `git_tokens:read` and `gateway:read` are in `organisation:write`, because members see token previews and gateway settings;
- reading the prompt-log settings checks `gateway:read`; reading and exporting the log checks `prompt_log:read`;
- listing and cancelling invitations check `members:write`, not `members:read` and `members:full`;
- access changes and recovering repository administration check the owner marker, not `audit:read`; attaching an existing graph to the organisation does too;
- seeing everyone's agent sessions checks `audit:read`; your own check `agent_sessions:read`;
- `ask_local` and `local_draft` as MCP tools check `repository.code:read`; the API endpoints the bridge calls check `repository.ai:write`;
- the brief plan checks `repository.code:read`;
- the member-repositories setting is read with `repositories:write` and changed with `repositories:full`; the repository default is read and changed with `repositories:full`;
- `rule_guide`, `security_symbols` and `license_report` check `repository.security:read`;
- `repository.memory:full` is held by `repository:write`, because writers clear memory today;
- the gateway's model routes (`/v1/...`) check `organisation.gateway:write`.

Some checks stay on roles until phase 6, because they compare two roles rather than ask for a permission: the job record's role, the invitation ceiling (nobody invites above their own role) and the rule that only owners make owners.

The rename followed as migration 0040: memberships, invitations, job records, SSO settings and daily statistics hold `guest`, `read`, `write`, `full` and `owner`; repository tuples `read`, `write`, `full` and `deny`; team tuples `read` and `full`, with `team#read` as the subject of team grants. It changes names only, so policy revisions stay as they were and projections need no rebuild (their `NONE`/`READ`/`WRITE`/`ADMIN` levels did not change). Responses use the new names; `accounts.ParseRole` (through `Role.UnmarshalJSON`) and `accounts.ParseTeamRelation` accept the old ones in requests for one release and log `deprecated role name in request` with the old and new name only. The OpenFGA model of phase 4 still calls the team relation `member`; phase 5 writes `team#read` as that relation or renames it in the model.

## Phases

Each phase is one pull request.

1. **This plan.**
2. **Permission table and names:**
   - routes and MCP tools check `resource.area:level` permissions through one table;
   - built-in roles are renamed to `resource:read|write|full` and `repository:deny` (Go, migration, API with old names accepted for one release, SSO mappings, web, docs);
   - the Access page lists effective permissions.

   No change in who can do what. `accountstest` proves it for every route and tool.
3. **Custom roles and scoped API keys:**
   - migration for roles and their grants;
   - API and Settings → Roles page;
   - no-escalation checks;
   - audit;
   - keys limited to a subset of permissions.
4. **OpenFGA model and client:**
   - the model file under `internal/authz/fga`;
   - a client that wraps the SDK with timeouts, `HIGHER_CONSISTENCY` and failing closed;
   - compose service and init step;
   - `just test-openfga` on a throwaway container, running the full ReBAC contract against OpenFGA;
   - OpenFGA added to the integration contracts job in CI.
5. **Outbox sync:** tuple writes from every policy change, synced-revision tracking, the freshness check, `fga rebuild` and `fga verify`, and metrics.
6. **Cutover:** `AccessOf`, route policies and MCP checks ask OpenFGA when the switch is on (list with `ListObjects`, single checks with `Check` or `BatchCheck`), and the docs move to the new model. The Go evaluator leaves production code.
7. **Helm subchart:** in the final deployment phase.

## Open questions

- **MCP write rights (recommendation from phase 4, for the owner to confirm):** the MCP server holds no OpenFGA key and asks the API, forwarding the caller's own API key as it does for `offload_turns`. The API is then the only process that can reach OpenFGA, and the only writer.
  - OpenFGA's access control (`OPENFGA_EXPERIMENTALS=enable-access-control`) can limit a client to reads, but it is marked experimental and not recommended for production, it works only with OIDC client credentials (the client is the token's `azp` or `client_id`), so every service would need an identity provider client, which goes against "no bundled identity provider", and its roles live in a separate access-control store that has to be bootstrapped by hand. Pre-shared keys cannot be scoped at all.
  - The cost is one internal round trip per tool call: the API answers a tool's whole need in one `BatchCheck` or `ListObjects`, and the MCP server already rechecks the caller with the accounts database on every call today. Decisions are never cached on the MCP side.
  - Workers need the same treatment: they hold no API token today, so in phase 6 they either get an internal check endpoint on the API (with the job's validated identity) or a key of their own, which would then be able to write. The first keeps "only the API holds a key"; it needs a decision before phase 6.
- How long should old role names stay accepted on API input? The plan says one release.
- Should custom roles be shareable across organisations, for example as platform-wide templates? The plan keeps them per organisation.
