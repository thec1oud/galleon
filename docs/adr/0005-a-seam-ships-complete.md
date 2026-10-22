# 0005. Ship every seam with a default adapter, a conformance suite and a fake

Status: Accepted
Date: 2026-10-20

## Context

If Galleon only defines interfaces, a team starting a project has to research,
choose and wire an adapter for every one of them. That is the Chi experience
with more interfaces, the opposite of complete. And "supports other tools"
means little if the only proof is that another tool compiles against the
interface.

## Decision

A seam without a default implementation is an incomplete component. Every
seam ships with all four of:

1. **The interface**, small and owned by Galleon.
2. **A default adapter** that works with zero configuration.
3. **A conformance suite** that any adapter, including a third party's, must
   pass to prove it is correct. Suites live in
   `testkit/conformance/<seam>`.
4. **A fake** for tests.

## Consequences

- Galleon's correctness guarantees live in the conformance suites, so they
  hold for every adapter, not just ours.
- A new seam costs more to add: four deliverables, not one.
- A user gets a working default for everything on day one.

## Alternatives considered

- **Interfaces only, adapters left to the ecosystem.** Faster to build, but
  pushes integration work onto every user and gives no correctness guarantee.

## Sources

- [Part 4: Pushback 1](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-1-a-seam-for-everything-is-not-the-same-as-complete)
- [Part 6: §12, conformance suites](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md#12-conformance-suites-how-theyre-delivered)
