# Overview and dependencies

## What needs attention

The **Overview** page ranks what to fix across your repositories, worst first:

- vulnerable dependencies your code calls
- critical and high security findings
- licence conflicts
- private repositories that lost access
- failing syncs
- repositories no security check covers
- stale indexes
- missing Renovate or Dependabot

Every item explains why it matters and links to where it is fixed. Each repository's Summary shows its own list.

## Dependencies page

A repository's **Dependencies** page lists every package with its licence, known vulnerabilities (and whether your code calls them) and update automation. See [Cleanup and licences](cleanup-licenses.md) for how licences are judged.
