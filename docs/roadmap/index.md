# Roadmap

!!! tip "In plain words"
    What is built lives under Features, Architecture and Administration. This tab holds the rest: the decisions behind what was built, and the ideas that are not built yet.

<div class="grid cards" markdown>

- **Feature decisions**

    Why CodeSynapse works the way it does. Architecture decision records (ADRs) and the design plans of features that are built, with the trade-offs that were weighed.

    [Plan and decisions](../PLAN.md) · [ADRs](../adr/0001-search.md)

- **Ideas**

    Work that is proposed or only partly built, and the questions that are still open.

    [Ideas and open work](ideas.md)

</div>

## How an idea becomes a feature

1. An idea gets a plan in `docs/plans/` and a line in [Plan and decisions](../PLAN.md). The plan says what changes, what stays out of scope and which security rules apply.
2. A choice that is hard to undo (a store, a protocol, what leaves the machine) gets an ADR in `docs/adr/`.
3. The feature is built in phases, one pull request each. The plan's status line says which phases are built.
4. Once built, the plan stays here as the record of why. The how-to moves to Features, Architecture or Administration.
