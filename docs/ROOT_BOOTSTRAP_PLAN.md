# Root bootstrap and separation of duties

Status: planned follow-up to SaaS phase 4d. This is a new restriction beyond the merged repository-permission work. Phase 4d removed implicit platform access, but a platform administrator with explicit organisation membership can still act on repository grants. The bootstrap root account must lose that ability when this follow-up is implemented. Phase 4e telemetry continues separately; root enforcement is not part of its completion claim.

## Identity and permitted work

Persist an immutable bootstrap-root account classification in PostgreSQL. It is separate from the mutable platform-administrator flag and from organisation roles. Neither granting platform administration to a standard user nor changing the root account's email changes this classification. Identify the existing bootstrap account from authoritative setup history during migration; ambiguous installations require an explicit audited migration decision, never a guess from names or email addresses.

The root account may manage global settings, platform upgrades and health, telemetry, security and audit logs, and system debugging or disaster recovery. These capabilities do not include reading repository symbols, summaries, reviews, memory or other tenant content. Recovery cannot impersonate a standard user or create a grant that lets root perform business work.

Root must not run ordinary workflows or create business data: repository registration or indexing, code queries, reviews, memory writes or recalls, architecture proposals, annotations, reuse records and organisation business settings are denied. Explicit organisation membership, a direct repository grant, team membership, an API key, a queued job or a platform-admin flag cannot bypass this restriction. Standard users continue to use ordinary membership and repository-permission checks.

## Initial setup and handoff

A root session that has not completed bootstrap enters a dedicated setup flow. Its immediate task is to provision an organisation and at least one distinct, active standard-user account associated with that organisation. Grant that standard user the organisation-owner role so the operator can complete normal setup after handoff. The root account does not become an organisation member or repository administrator as a side effect.

Create the organisation, standard account, owner membership, setup completion state and audit event in one transaction. Validate the password through the existing account policy; never log, return or persist clear-text credentials beyond the operator's submitted request. Keep completion idempotent so retries cannot create extra accounts or organisations. Concurrent submissions must commit at most one completed bootstrap. Failure leaves a retryable setup state without partial ownership.

After successful provisioning, end the root setup session and clear its browser cookie. Show: “Setup is complete. Sign in with the standard account for organisation work.” Do not mint a standard-user session or automatically impersonate that account. The operator must authenticate with the new credentials. Later root logins open the administration area and retain only governance capabilities. Setup completion cannot be reset through ordinary user APIs; a disaster-recovery procedure must be explicit and audited.

For existing installations, require a verified active standard owner before completing migration or revoking the final working setup path. Do not silently convert a root session into a standard session. Retain an audited governance recovery path if the standard owner loses access, while still requiring a fresh standard-user login for business tasks.

## Enforcement

Add the root restriction to the shared accounts authorization layer and enforce it before repository or organisation business checks. API route policies and the MCP tool policy table must classify every action explicitly. Unknown actions fail closed. HTTP and stdio MCP authentication, resumed sessions and queued/background work must recheck the account classification. A root credential must never authorize deferred tenant work.

The only root tenant mutation exception is the narrow initial-bootstrap transaction. Account suspension and credential recovery remain governance actions, with existing last-active-admin protections. Organisation permission recovery must target a distinct standard user and must not disclose tenant content or give root a business session.

Return a consistent permission error for an ordinary action attempted as root: “Root accounts cannot perform organisation work. Sign out and sign in with a standard account in your organisation.” Audit the denied action without logging credentials or request content. The UI hides business navigation and provides the sign-out action; backend enforcement remains authoritative.

## Acceptance checks

- Fresh setup cannot finish without an organisation and an active standard owner. Invalid credentials, audit failure and concurrent retries leave no partial or duplicate bootstrap records.
- A completed bootstrap revokes the setup session, clears its cookie and requires a real standard-user login. Refreshing the completion page cannot restore the session.
- Root cannot execute any business API route or MCP tool, including read-only tools, with direct grants, team grants, organisation membership, API keys, resumed sessions or forged tenant fields.
- Jobs queued before a root restriction or migration are denied before execution and before persistence or push. Standard-user jobs continue under their own current permissions.
- Root can use the explicit governance allowlist, inspect scoped platform telemetry and audit logs, and perform audited account recovery without receiving repository contents.
- Changing the mutable platform-admin flag never changes bootstrap identity. Migration ambiguity fails closed without locking operators out of the documented recovery procedure.
- Single-tenant/offline operation has an explicit documented applicability rule; it must not quietly treat a root credential as unrestricted offline access.

Complete this follow-up with account-store contracts, API/MCP authorization tests, UI handoff tests, migration and recovery tests, then commit, push and open its own pull request. Kubernetes work stays in the final deployment phase.
