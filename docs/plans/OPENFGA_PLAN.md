# OpenFGA authorisation plan

Status: phases 1 to 6 done (2026-11-03): with `CODESYNAPSE_AUTHZ=openfga` the API decides access with OpenFGA, and workers and the MCP server ask the API. Phase 7 (the Helm subchart) waits for the deployment phase.

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
    define owner: [user, team#read]
    define full: [user, team#read] or owner
    define write: [user, team#read] or full
    define read: [user, team#read] or write
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
    define read: [user] or full

type repository
  relations
    define organisation: [organisation]
    # Set only for organisation-space entries, never personal ones.
    define org_space: [organisation]
    define direct_full: [user]
    define direct_write: [user]
    define direct_read: [user]
    define direct_deny: [user]
    # A custom role grant marks the user or team (also when the role is gone)
    # and writes the role's permissions in the user_ or team_ tier below.
    define direct_custom: [user]
    define has_direct: direct_full or direct_write or direct_read or direct_deny or direct_custom
    define team_full: [team#read]
    define team_write: [team#read]
    define team_read: [team#read]
    define team_custom: [team#read]
    define has_team: team_full or team_write or team_read or team_custom
    define default_read: default_readers from org_space
    # A direct grant replaces team grants and the default, even when weaker;
    # a team grant replaces the default.
    define full: (direct_full or (team_full but not has_direct)) and active from organisation
    define write: (full or direct_write or (team_write but not has_direct)) and active from organisation
    define read: (write or direct_read or ((team_read or (default_read but not has_team)) but not has_direct)) and active from organisation

    define user_reviews_write: [user]
    define team_reviews_write: [team#read]
    # One trio like this per permission: its floor role, the level above it
    # in the same area, or a custom role's grant.
    define reviews_write: (write or reviews_full or user_reviews_write or (team_reviews_write but not has_direct)) and active from organisation
```

The per-permission relations are generated from the Go permission table (`TestModelFiles` in `internal/authz/fgasync` writes `model.fga` and `model.json` with `-update-model` and fails when either differs), so the model and the table cannot drift apart. The built-in `read`/`write`/`full` relations answer for built-in roles only; for a decision, phase 6 asks the permission's relation (`code_read`, `reviews_write`, …), which holds custom grants too.

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

`codesynapse-api openfga-rebuild` has the API write every tuple again from PostgreSQL, and `openfga-verify` compares the two stores. Both are audited. See "Phase 5 as built" for how.

## Running OpenFGA

- **Compose:** an `openfga` service with its own database and user in the existing PostgreSQL. Migrations run as a separate init step. It is reachable on the internal network only, never published to the host.
- **Authentication:** a pre-shared key per service from the environment (`CODESYNAPSE_OPENFGA_TOKEN`) for the API, the workers and the MCP server, which computes access itself today. The playground and the unused port are switched off. Only the API writes tuples. OpenFGA's pre-shared keys are not limited to reads, so the MCP server's key could write too. See the open questions.
- **Store and model IDs:** created by the API at start-up (like `EnsureSchema`), recorded in PostgreSQL, and checked by every other process.
- **Telemetry:** check latency, errors and outbox lag are `platform_*` metrics, with no tenant, user or object labels. Spans carry the relation and object type only, never IDs.
- **Helm:** an `openfga` subchart, on by default and with its own `values.schema.json`, like the other stores. Per AGENTS.md, this waits for the final deployment phase. Until then, the chart keeps the Go evaluator, and the switch below stays off there.
- **Switch:** `CODESYNAPSE_AUTHZ=openfga` turns OpenFGA on in the API (compose sets it). Without the switch, or with `postgres`, the Go evaluator decides. It stays in production code for that mode and the publication-lock exception, and as the test reference (see "Phase 6 as built").

New dependencies, all Apache-2.0: the `github.com/openfga/go-sdk` Go module and the `openfga/openfga` image (pinned by digest). The test container uses the same image.

### Phase 3 as built

- **Storage:** migration 0042 adds `custom_roles` (organisation, name unique ignoring case, description, resource, permissions as a text array, creator, timestamps). Grants are ordinary tuples whose relation is `role:<id>`: on a repository object for a user or `team#read`, and on the organisation object for a user or a team. Deleting a role while any tuple names it answers 409. Phase 6 mirrors them into OpenFGA (see "Phase 6 as built").
- **Evaluation:** permission sets are a 64-bit set (`accounts.Perms`). A direct grant, built-in or custom, wins on a repository; otherwise team grants are united (the strongest built-in one as before, plus every custom one); otherwise the default. A grant naming a missing role gives nothing, with no fallback. A repository is reachable only with `repository.code:read`. Custom organisation roles add to the built-in role (`Service.PermsIn`); `Require` checks the built-in bundle first, so owners and admins cost no extra read.
- **Keys:** `api_keys.scope` holds the key's permissions; `Service.KeyLimit` re-reads the key on every check (a revoked or expired key reaches nothing) and `AccessFor` intersects the owner's access with it. The MCP server goes through the same `AccessFor`.
- **Deviations, the safer choice in each case:** no organisation-wide grant of repository roles (the plan allowed one that works like the default); the job role, the invitation ceiling and owner-making stay on built-in roles until phase 6; repository roles and key scopes with repository permissions always include `repository.code:read`.

### Phase 4 as built

- **Model format:** `internal/authz/fga/model.fga` is the source people read and edit; `model.json` beside it is the same model in OpenFGA's JSON form, made once with OpenFGA's own transformer (`fga model transform`) and embedded in the binary. No DSL parser ships: `github.com/openfga/language` stays out. `TestModelDSLMatchesJSON` renders the JSON back to DSL and compares every relation, so neither file changes alone. When phase 2's permission table lands, a generator writes both files and the test keeps them honest.
- **Client** (`internal/authz/fga`): configured from `CODESYNAPSE_OPENFGA_URL` and `CODESYNAPSE_OPENFGA_TOKEN` only; https anywhere, http only to a single-label or cluster-local name (the rule of `telemetry.CheckAlertsURL`); no proxy, no redirects, a 2 s timeout on every call, one retry. `Check` and `BatchCheck` (at most 50) ask for `HIGHER_CONSISTENCY`; any error, missing answer or per-item error makes every answer false. `ListObjects` refuses an answer of `ListLimit` (1000, OpenFGA's own cap) or more as truncated. `WriteTuples`/`DeleteTuples` send at most 100 tuples in one transaction and ignore duplicates and missing deletes, so a replayed outbox batch is harmless. `EnsureStore`/`EnsureModel` find or create the store by name and write the model only when the stored one differs. The SDK's own telemetry is off: its attributes carry store and model IDs.
- **Contract:** `just test-openfga` starts a throwaway OpenFGA (in-memory datastore, a random pre-shared key) and runs `internal/authz/fga/contract_test.go`: every row of the table above that the model decides, plus idempotent writes and `EnsureModel` reusing the stored model. It lives beside the client rather than in `accountstest`, which phase 2 is reshaping; phase 6 runs the same rows against the Go reference there.
- **Compose:** `openfga-db-init` (the postgres image's `psql`) creates the `openfga` user and database idempotently and closes the database to everyone else; `openfga-migrate` runs OpenFGA's migrations; `openfga` runs with pre-shared keys, playground, metrics and profiler off, gRPC bound to 127.0.0.1 (the HTTP API reaches it in-process; the health check uses it), no published port. `just init` adds `OPENFGA_DB_PASSWORD` and `OPENFGA_API_KEY` (the one key OpenFGA accepts; see the open questions) to `.env`. Nothing reads them in the API, workers or MCP server yet.
- **Deviations:** `OPENFGA_LIST_OBJECTS_DEADLINE` is 5 s, above the client's 2 s timeout, so OpenFGA never cuts a list short and returns it as complete (it does so silently at its deadline); OpenFGA refuses a deadline above its 6 s request timeout. The table's "above `MaxTuples`" is `ListLimit`. One key instead of one per service, following the recommendation below.

### Phase 5 as built

Nothing decides with OpenFGA yet: PostgreSQL and `AccessOf` keep deciding every request, and users see no change.

- **Sync** (`internal/authz/fgasync`): for one organisation, `Desired` turns its policy into every tuple the model needs:
  - memberships go to `member` and their level, guests to `guest`, and the default to `default_readers` (READ_ONLY only);
  - each team is anchored to its organisation, with its organisation role as `team#read` on that level, members as `read` and maintainers as `full`;
  - each repository is anchored to its organisation, with `org_space` for organisation entries only (personal entries get no default), and grants as `direct_full|write|read|deny` or `team_full|write|read`.

  The policy comes from `accounts.(*Service).OrgPolicy` through one read snapshot. It holds only what `AccessOf` would count:
  - members, but not suspended accounts or the bootstrap root;
  - team members, for teams of the organisation and only for eligible members;
  - the organisation's entries, but not lost ones.

  The sync reads what OpenFGA holds for the organisation, its anchored teams and repositories, and every object the policy names, at most 8 objects at a time and bounded per object. It deletes what is not wanted and writes what is missing, in batches of 100. Non-anchor deletes go first and anchor deletes after them, then anchor writes and the rest, so a sync that stops halfway never strands tuples on an object a later sync cannot find. Only then does it record the revision. It is idempotent and safe to replay: a sync that is cut short leaves the old revision recorded, and the next one carries on from what OpenFGA then holds. A policy `Desired` cannot express (an unknown role, an ID with `:` or `#`) fails that organisation's sync as a whole, so its revision stays stale. The other organisations go ahead.
- **Revisions:** every policy change already raised `orgs.policy_revision` through `policyChange`. Suspending or resuming an account and the platform-admin flag carried no organisation, so they raised none; they now raise the revision of every organisation the user belongs to (`touchUserOrgs`). The bootstrap root's classification (setup, `designate-root`) still raises none. The start-up rebuild covers it, and phase 6 keeps the Go `businessAllowed` and suspension checks in front of OpenFGA.
- **Watermark** (migration 0041, `openfga_sync`): one row per organisation with the store, the model, the synced revision, when it was synced and when a rebuild was last asked for. It is separate from the projection outbox because it records what OpenFGA accepted, not what was queued. `OpenFGAPending` lists organisations with no row, or whose revision, store or model differ, or with a rebuild asked for since the last sync. `RecordOpenFGASync` never lowers a revision and clears a rebuild request only when the sync started after it.
- **Freshness:** `fgasync.Fresh` returns `ErrStale` unless the row names the client's store and model and a revision at least the organisation's. Phase 6 calls it before every check.
- **Where it runs:** `syncOpenFGA` (`services/api/internal/api/openfga.go`) is one of the `api-loops`, so only the process holding the lease writes. It selects or creates the `codesynapse` store and model, retrying with backoff while OpenFGA starts. It asks for a rebuild of every organisation each time it starts, so a new leader repairs any drift. Then it syncs at most 50 pending organisations every 15 s, each pass bounded to 40 s, well under the lease's minute. Each OpenFGA call has its own 2 s timeout. Workers and the MCP server hold no key: compose sets `CODESYNAPSE_OPENFGA_URL`/`_TOKEN` on the API only and blanks them for workers, and `serve` reads them only in an API process with accounts.
- **Rebuild and verify:** `POST /api/v1/admin/openfga/rebuild[?org=ID]` (platform admin, signed-in session, never an API key) and `codesynapse-api openfga-rebuild [--org]` record `openfga.rebuild` in the audit log and then only mark organisations for the loop. Neither writes to OpenFGA outside the lease. `codesynapse-api openfga-verify [--org]` records `openfga.verify` and compares without writing tuples. It prints one JSON line per organisation and exits non-zero on drift.
- **Repository objects:** `repository:<id>`, where the ID is the hex of the first 16 bytes of SHA-256 over the organisation ID, `|` and the graph name. Graph names may contain characters OpenFGA IDs cannot, and the object then names no repository.
- **Metrics:** `platform_codesynapse_openfga_syncs_total{outcome}`, `platform_codesynapse_openfga_tuples_total{op}` and the `platform_codesynapse_openfga_pending_orgs` gauge, with no tenant, user or object labels.
- **Tests:** `fgasync` unit tests use a fake client that refuses duplicate writes and missing deletes, stricter than the real client (which ignores both), so a sync that writes what is already there fails the test. They cover the tuples per policy, drift repair, idempotence, the batch order, a failure halfway and a failing organisation among good ones. `accountstest.RunOpenFGASync` runs the watermark contract on memstore and PostgreSQL. `TestParityWithAccessOf` (`just test-openfga`) applies real policy changes in sequence: grant, revoke, team change, team member removed, deny, default change, suspension, member removal, personal entry, lost entry, restore and team deletion. After each sync it checks that OpenFGA's `read`/`write`/`full` on every repository, and every organisation level and `active`, equal `AccessOf` for every user, and that `Verify` finds no drift.

### Phase 6 as built

Phase 6 lands in three pull requests: custom roles in OpenFGA, the internal check endpoint, and the cutover.

- **Custom roles in OpenFGA:** a grant of `role:<id>` is mirrored as the role's permissions, read from `custom_roles` at every sync, so changing or deleting a role raises the policy revision and the next sync rewrites every grant of it.
  - On a repository, a user's grant writes `direct_custom` plus `user_<permission>` for each permission; a team's writes `team_custom` plus `team_<permission>` with `team#read` as the subject.
  - On the organisation, a member's or team's role writes `<permission>` (`audit_read`, …) beside the built-in level; each organisation permission relation is its floor level, the level above it in the area, or such a grant, and always `active`.
  - The markers `direct_custom` and `team_custom` are written even for a role that grants nothing (gone, of another organisation or of the other resource), because the grant still replaces team grants and the default, as in `AccessOf`.
  - A role holding a marker or a permission of the other resource fails that organisation's sync, so it stays stale.
  - `accounts.ErrCustomRolesNotMirrored` is gone.
  - `TestParityWithAccessOf` now compares every permission relation on every repository and organisation with `RepoAccess.Perms` and `OrgPerms`, through custom grants (direct over team, teams adding up, a missing role, another organisation's role), a role change and a role deleted while granted, and pins a few `AccessOf` decisions so the fixtures keep meaning what they say.
- **Internal check endpoint:** workers and the MCP server ask the API about access, so after the cutover only the API needs to reach OpenFGA. The API still answers with the Go evaluator. The cutover changes only what is behind the endpoint.
  - **One question:** `accounts.AccessQuery` names one organisation and either at most `accounts.MaxAccessGraphs` (50) graphs, none (every repository of the organisation) or `org_only` (levels and permissions only). `Service.AccessIn` checks the API key's organisation binding before asking, and answers the access filtered to that organisation and those graphs, with team IDs left out. With `Service.Remote` set, it asks the API, refuses an answer about another user, and intersects the answer with the key's scope locally. Errors refuse; nothing is cached.
  - **Callers:** `POST /api/v1/internal/access` (`anyone`, but the handler needs an API key or a signed-in session plus a registered runtime session of that user and organisation) answers for the caller only. The MCP server (`mcpserver.APIAccess`) sends the calling agent's own key and runtime session headers, never the installation token, so `AccountsAccess.Reach` is one audited request per tool call. In accounts mode the MCP server refuses to start without `CODESYNAPSE_API_URL` (since the cutover; it logged a warning and decided locally before).
  - **Jobs:** `POST /api/v1/internal/jobs/{id}/access` takes no credentials. The worker proves the job's lease instead (`X-CodeSynapse-Job-Lease`, the process's 16-byte random owner, compared in constant time with an active job's owner and lease). The API rebuilds the job's identity with `jobContext`, which rechecks session, account, key, membership and permission and audits a denial. It answers only for the job's own user and organisation and only about the job's repository. Workers read `CODESYNAPSE_API_URL` (compose sets `http://api:8080`); in accounts mode a worker without it refuses to start (since the cutover). The job ID travels in the context (`withJobID`), and child jobs queued from a job ask with the parent's lease.
  - **Deviations, the safer choice in each case:** checks made under the publication policy lock (`publishRepo`) read the locked snapshot locally, because asking the API there would wait on the lock the job holds (memory adapters) or answer from outside the transaction. The cutover keeps them that way (see below). A job's follow-up work (`Ingested`, `After`) now runs before the job is marked finished, so its lease still proves it. The job owner grew from 8 to 16 random bytes because it is now a credential. The Helm chart does not set `CODESYNAPSE_API_URL` for workers yet (phase 7).
- **Cutover:** with `CODESYNAPSE_AUTHZ=openfga` (compose sets it for the API; unset or `postgres` keeps the Go evaluator; any other value, or `openfga` without `CODESYNAPSE_OPENFGA_URL` and `_TOKEN`, refuses to start) the API decides with OpenFGA (`fgasync.Decider` as `Service.Decider`).
  - **Go in front:** `AccessOf` still reads the PostgreSQL snapshot first. It refuses suspended accounts, the bootstrap root (`businessAllowed`) and too large or invalid policy, and it names the active memberships and usable entries. Only those are asked about, so OpenFGA never adds a member or a repository PostgreSQL does not have. `RoleIn` keeps the key's organisation binding, its credentials and the membership check, then takes the role and organisation permissions from OpenFGA, owners included. The key scope is intersected after the decision (`KeyLimit`).
  - **Questions:** per organisation, one `BatchCheck` of the levels (`read`, `write`, `full`, `owner`), `active` and every organisation permission; then `code_read` on each entry and the other repository relations on the reachable ones only. Up to 400 checks go as `BatchCheck` in chunks of 50, at most four in flight. Larger questions use `ListObjects` per relation, filtered to the entries asked about. A truncated list (`fga.ErrTruncated`) is checked one by one when at most 1000 entries are asked, and refused otherwise.
  - **Failing closed:** every organisation passes `fgasync.Fresh` first. A question about one organisation (`AccessIn`, `RoleIn`, `Require`, `PermsIn`) that is stale answers `accounts.ErrPolicyStale`: the API answers 503 with `Retry-After: 2`, and a worker turns that 503 back into `ErrPolicyStale`. In a question about every organisation, a stale one is left out. An OpenFGA error, a missing store or another model refuses. The store and model are selected without creating anything (`fga.Client.Select`: exactly one store by name whose newest model is ours), kept 30 s and retried after 5 s; a failed reselect keeps the previous one. The sync loop runs every 2 s instead of 15 s, so a policy change is decided within seconds.
  - **Workers and the MCP server:** in accounts mode they refuse to start without `CODESYNAPSE_API_URL`, and a worker that has none anyway refuses every access question (`refuseAccess`). Single-tenant and offline mode need neither.
  - **The one exception:** checks under the publication policy lock (`publishRepo`) keep reading the locked PostgreSQL snapshot with the Go evaluator. OpenFGA cannot see a transaction that has not committed, and asking the API there would wait on the lock the job holds.
  - **Deviation:** the Go evaluator stays in production code as the `postgres` mode, for the lock exception and to explain a grant (`Via`, `CustomRole`). When it and OpenFGA disagree, OpenFGA's answer stands and the API logs a warning with the count only.

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

The rename followed as migration 0040: memberships, invitations, job records, SSO settings and daily statistics hold `guest`, `read`, `write`, `full` and `owner`; repository tuples `read`, `write`, `full` and `deny`; team tuples `read` and `full`, with `team#read` as the subject of team grants. It changes names only, so policy revisions stay as they were and projections need no rebuild (their `NONE`/`READ`/`WRITE`/`ADMIN` levels did not change). Responses use the new names; `accounts.ParseRole` (through `Role.UnmarshalJSON`) and `accounts.ParseTeamRelation` accept the old ones in requests for one release and log `deprecated role name in request` with the old and new name only. Phase 5 renamed the model's team relation `member` to `read` to match.

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
5. **Outbox sync (done):** tuple writes from every policy change, synced-revision tracking, the freshness check, `openfga-rebuild` and `openfga-verify`, and metrics.
6. **Cutover:** `AccessOf`, route policies and MCP checks ask OpenFGA when the switch is on (list with `ListObjects`, single checks with `Check` or `BatchCheck`), and the docs move to the new model. The Go evaluator leaves production code.
7. **Helm subchart:** in the final deployment phase.

## Open questions

- **MCP write rights (recommendation from phase 4, for the owner to confirm):** the MCP server holds no OpenFGA key and asks the API, forwarding the caller's own API key as it does for `offload_turns`. The API is then the only process that can reach OpenFGA, and the only writer.
  - OpenFGA's access control (`OPENFGA_EXPERIMENTALS=enable-access-control`) can limit a client to reads, but it is marked experimental and not recommended for production, it works only with OIDC client credentials (the client is the token's `azp` or `client_id`), so every service would need an identity provider client, which goes against "no bundled identity provider", and its roles live in a separate access-control store that has to be bootstrapped by hand. Pre-shared keys cannot be scoped at all.
  - The cost is one internal round trip per tool call: the API answers a tool's whole need in one `BatchCheck` or `ListObjects`, and the MCP server already rechecks the caller with the accounts database on every call today. Decisions are never cached on the MCP side.
  - Workers need the same treatment: they hold no API token today, so in phase 6 they either get an internal check endpoint on the API (with the job's validated identity) or a key of their own, which would then be able to write. The first keeps "only the API holds a key"; decided for phase 6: the internal endpoint, with the job's lease as proof (see "Phase 6 as built").
- How long should old role names stay accepted on API input? The plan says one release.
- Should custom roles be shareable across organisations, for example as platform-wide templates? The plan keeps them per organisation.
