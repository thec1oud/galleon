# 0019. Define a tool-neutral migration seam: a small core plus optional capabilities

Status: Accepted
Date: 2026-10-20

## Context

Migration tools differ widely. Versioned tools (goose, golang-migrate) list
unapplied files; declarative tools (dpg, Atlas) diff against a desired state,
can plan offline, classify risk and detect drift. A single fat interface
would force every tool to fake abilities it lacks. dpg's own design,
including data migrations, is out of Galleon's scope.

## Decision

Package `migrate` defines a small required core, `Migrator` (`Name`,
`Status`, `Apply`), plus optional capabilities discovered with a type
assertion, the way `http.Flusher` works: `Planner`, `OfflinePlanner`,
`Verifier`, `Backgrounder` and `Completion`. Risk is one shared scale
(Safe, Caution, Destructive, Manual, Unknown).

Around any migrator, Galleon provides the same behaviour: apply exactly once
across the cluster under the coordination lock, a readiness gate on schema
status, a per-environment risk policy with plan-fingerprint matching,
several migrators in a declared order, and a span and log per step. The
built-in implementation for plain SQL files is `pg/sqlmigrate`.

## Consequences

- Any tool can plug in by implementing three methods, and gains more
  behaviour by implementing more capabilities.
- Galleon says clearly when a capability is missing instead of faking it.
- Background tasks depend on the job system, which comes later.

## Alternatives considered

- **Versioned files only ("list pending, apply").** Doesn't fit declarative
  tools like dpg.
- **One interface with every ability.** Forces tools to fake what they can't
  do.

## Sources

- [Part 4: decision D12 and Round 3](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#round-3-the-migration-seam-j3)
