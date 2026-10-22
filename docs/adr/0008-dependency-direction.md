# 0008. Keep tools independent of Galleon, and put adapters in their own modules

Status: Accepted
Date: 2026-10-20

## Context

The tools Galleon plugs into (dpg, an ORM, vantage, comms, billing, Themis)
are meant to be standalone and usable from anything. If Galleon imported one
and it imported Galleon, neither could be used alone.

## Decision

- Tools never import Galleon.
- Galleon's core never imports a tool, a battery or an adapter.
- Each seam's interface lives in Galleon. Each adapter is a small separate Go
  module under `adapters/<tool>` that imports Galleon's core and that one tool.
- A battery imports only the core's public API, never its internals.
- Nothing imports `testkit` except tests.

These rules are checked in CI by import-graph tests.

## Consequences

- Each tool stays usable without Galleon, and Galleon stays usable with a
  different tool.
- A user who doesn't use a tool never downloads it.
- The default project template wires the first-party adapters in, so the
  out-of-the-box experience still has everything.
- More modules to version and release.

## Alternatives considered

- **Adapters inside the core module.** Simpler to release, but every user
  would pull every tool's dependencies.

## Sources

- [Part 4: Pushback 6](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-6-the-dependency-direction-between-galleon-and-your-systems)
- [Part 5: §2, module layout and dependency rules](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#2-module-layout-and-dependency-rules)
