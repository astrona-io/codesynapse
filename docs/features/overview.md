# Overview and dependencies

!!! tip "In plain words"
    The Overview page answers "what should I look at first?" across all your repositories, worst problem first. The Dependencies page lists the libraries a repository uses, with their licences and known security holes.

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
