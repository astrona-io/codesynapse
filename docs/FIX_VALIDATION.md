# Generated fix validation

Phase 4j.1 enforces validation policy **1** in `services/api/internal/fixes`.
Fix branches remain opt-in through `CODESYNAPSE_ALLOW_PUSH=true`. The model must
be local; an explicit `send_source` override cannot enable cloud generation.
Every successful branch still needs CI and human review before merging.

## Accepted changes

An accepted comment must identify one Go function or method by physical line
range. The generated file must parse as Go and preserve every byte outside the
selected function bodies. This protects signatures, receivers, imports, file
headers, declaration order, unrelated code and line endings outside edited bodies. `//line` directives
cannot change the physical ranges used for validation. Multiple selected bodies
in one file are supported.

Signatures, imports, package/type/variable declarations, comments outside function bodies
and non-Go files require manual edits. Ambiguous same-line declarations or a
comment spanning multiple declarations are refused. Valid syntax does not prove
type correctness, safe behavior or that every changed statement is necessary.
The validator does not run repository code, external compilers or downloaded
language tools.

The response must be one code block, with no surrounding prose or extra blocks.
The runner accepts plain or `go` backtick fences of length 3–8. Files are capped
at 256 KiB; the response envelope adds at most 256 bytes. Empty, unchanged and
implausibly large rewrites are refused. Extraction preserves the returned bytes, and validation preserves original bytes
outside selected bodies;
for a source file without a final newline, the fence's terminating newline is
removed before comparison.

## Revision and publication checks

- A review needs a full immutable `Commit` or `Head` hash. If both are present,
  both must match the requested indexed head. `Base` describes the old side of
  a review diff and need not match. Missing, symbolic, abbreviated or conflicting
  source revisions require a new review; the runner does not guess a revision.
- The API rejects a stale review before queueing. Requests carry validation
  version 1; unknown versions fail rather than inheriting weaker checks.
- Before generation, commit and push, the runner rechecks authorization, the
  clone's head, the current indexed revision, review scope/revisions and the
  selected comments. Changed decisions, messages or ranges invalidate the run.
- Source files must match the pinned Git blobs. Reads are bounded and reject
  symlink files or directories, even when they point inside the worktree.
  Original hashes and modes are checked again before writes; validated output
  hashes and modes are checked before committing.
- The resulting commit must have the reviewed revision as its sole parent,
  exactly the expected changed paths, regular-file modes and matching blob
  hashes. HEAD must still name that commit immediately before push. Publication
  keeps the existing separate-worktree, new-branch and no-force restrictions.

These checks detect changes at explicit execution boundaries. They are not a
distributed transaction with a remote Git provider or protection against a
privileged local process racing the final syscall. Repositories and worktrees
must retain their existing process/filesystem isolation.

On rejection, inspect the failure, sync if needed, review the current revision
and queue a new request. Queued work remains in memory and is recovered as failed
after restart. Policy 1 appears in generated commit messages. To roll back
automation, set `CODESYNAPSE_ALLOW_PUSH=false`; do not bypass validation to retry
a refused edit.

## Diff parsing and resource bounds

`gitx.ParseDiff` now keeps rename/copy-only, binary and mode-only changes, decodes
Git-quoted paths and preserves significant filename spaces. Both hunk ranges
use checked integer arithmetic. Payload lines resembling file headers cannot
replace a path while they belong to the current hunk. Bad headers, unsafe paths,
oversized lines and inputs over 32 MiB reject the whole diff.

Header-only hunk summaries remain supported for impact analysis. This parser
does not establish that a patch can be applied to a file. Local `gitx.Run`
captures at most 32 MiB stdout and 64 KiB stderr, returning an error rather than
partial output when either limit is exceeded.

Regression tests cover real Git quoting, Unicode, rename metadata, malformed
hunks, overflow, repeated text, stale reviews, model-time changes, oversized
output, symlink aliases and commit contents/modes/parents. Parser and rewrite
fuzz targets exercise malformed input. No graph schema or dependency changes
are required.
