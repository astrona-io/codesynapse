# Architecture catalog

!!! tip "In plain words"
    Teams write down decisions ("we use PostgreSQL for accounts") in files next to the code. CodeSynapse reads those files so you or an assistant can ask: what did we decide, is it still true, and which code follows it? The answer also says which records are firm rules and which are only proposals.

Decisions, standards and designs usually live in files next to the code. CodeSynapse indexes them on every sync, so people and coding agents can ask "what did we decide, is it still true, and which code carries it out?" and get an answer that says which record is a rule and which is only a proposal.

## What is a record

Markdown files (and `.mmd`, `.puml`, `.d2`, `.drawio` diagrams) in the usual places become records:

| Where | Type |
|---|---|
| `adr/`, `adrs/`, `docs/adr/`, `decisions/` | `adr` |
| `rfc/`, `rfcs/`, `proposals/` | `rfc` |
| `standards/` | `standard` |
| `policies/`, `policy/` | `policy` |
| `runbooks/`, `playbooks/` | `runbook` |
| `architecture/`, `ARCHITECTURE.md` | `overview` |
| diagrams under `architecture/`, `diagrams/` | `diagram` |

A `type:` in the front matter places any Markdown file elsewhere. `README`, `index` and template files are not records.

## Writing one

Metadata goes in YAML front matter. The formats people already use are read too: `- Status: accepted` bullets under the title and a `## Status` section.

```markdown
---
id: ADR-0007            # optional: the number in the file name gives ADR-0007 anyway
status: accepted
owner: Platform team
components: [api, store]
labels: [security]
topic: storage          # what the record decides
supersedes: ADR-0002
implements: [internal/store, store.Reader, services/api/internal/api]
relates: [0003-cache.md]
conflicts-with: [ADR-0011]
---
# Use PostgreSQL for accounts
```

`implements` names the code that carries the decision out: a file, a directory (its packages), a package path or suffix, or a symbol by qualified or short name. A name that matches several symbols links nothing and is reported as ambiguous.

Only a short redacted summary of the text is stored. The text stays in the file; the catalog gives the lines to read, the content fingerprint and the commit it was indexed at.

## Lifecycle: what binds

Each type has its own statuses, and words are mapped onto them (approved is accepted, in review is in-review):

| Type | Statuses |
|---|---|
| ADR | proposed, accepted, rejected, deprecated, superseded |
| RFC | draft, in-review, accepted, rejected, withdrawn, implemented, superseded |
| Standard, policy | draft, active, deprecated, retired, superseded |
| Overview, runbook, diagram | draft, current, outdated, retired (current when nothing is said) |

A status that does not belong to the type, or none, is `unknown` and is reported.

**Only an accepted ADR, or an active standard or policy that nothing has replaced, is a rule.** Drafts, proposals, rejected, deprecated and superseded records, RFCs, overviews, runbooks and diagrams inform and are never presented as requirements. A record that an accepted record supersedes is not binding even if its own status was never updated; that mismatch is reported.

## Things for a person to look at

The catalog reports and never decides:

- a reference that matches no record or no code;
- a record replaced by another but not marked superseded, and a superseded record that names no successor;
- a replacement that is itself only proposed, so nothing is in force;
- supersession cycles;
- two rules in force for the same topic and component, and records that declare `conflicts-with`;
- statuses that are missing or unknown.

## Where

- **Web**: the **Architecture** page of each repository (filters, rules in force, problems) and a detail page for each record: lifecycle, supersession chain, code that carries it out, code its text mentions, sections with line ranges, related records and recent commits.
- **MCP**: `architecture_search`, `architecture_get`, `architecture_context` (give files, symbols, components and the task; rules in force come apart from background, with the problems found) and `architecture_issues`. Agents are told to read this before designing or changing code and to report conflicts instead of resolving them.
- **API**: `GET /api/v1/repos/{repo}/architecture` (also `/record?id=`, `/issues`, `/context`).

All of them go through one service on a reader fenced to the repository the caller may read, so search, relationships and summaries cannot show another repository's records.

## Proposals and links

People and agents can add to the catalog without editing files, and none of it is a decision.

- **Proposals.** A draft for a new ADR, RFC, standard or policy: title, text, topic, components, the records it would replace or relate to, and the code it would govern. It is stored apart from the repository's records, always with the draft status of its type (`proposed` for an ADR, `draft` for the others), is never binding and has no way to be marked accepted. Records it names must exist, and each code reference must name exactly one place. **Download file** gives the Markdown to commit (front matter with the draft status, the next free number and a suggested path); a person turns it into a decision by merging it with an accepted status. When a record with that title appears after a sync, the proposal is shown as merged. Proposals can be withdrawn and stay as history.
- **Links.** *Implements* (a file, package or symbol carries out a record), *relates* (two records) and *applies-to* (a record governs a component). The record must exist and an implements target must name exactly one place. A link is shown beside what the file declares, labelled with who added it (the MCP client or the user); it counts for `architecture_context` like a declared one, can be removed, and is flagged "needs re-linking" if the code is renamed or removed instead of silently disappearing.
- **Who and where.** Writing needs the Member role on the repository (viewers read). Proposals and links belong to the organisation that made them: two organisations that share one code graph never see each other's. Text is redacted and size-limited, every item records its client and user, and in accounts mode each MCP change is written to the audit log first (if that fails the change is refused). `--allow-catalog=false` (`CODESYNAPSE_ALLOW_CATALOG=false`) removes the two write tools; the web API keeps its own role checks.
- **MCP.** `architecture_propose`, `architecture_link` and the read tool `architecture_proposals`. Agents are told to check proposals first, to say what they proposed, and that they cannot approve anything.
- **API.** `GET/POST .../architecture/proposals`, `GET .../proposals/{id}`, `POST .../proposals/{id}/withdraw`, `GET .../proposals/{id}/export`, `GET/POST .../architecture/links`, `DELETE .../architecture/links/{id}`.

## Not in this version

Annotations, code identity across renames, reuse records, dependency replacement analysis and the unified search are the next steps of the same plan (`docs/ARCHITECTURE_CATALOG_PLAN.md`).
