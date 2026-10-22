# 0010. Validate everything at boot, before a node reports ready

Status: Accepted
Date: 2026-10-20

## Context

Many framework errors surface only when the first request hits a broken
route, or when a scheduled job runs at 3 a.m. against a missing dependency.
Most of them could have been found when the process started.

## Decision

Wiring, routes, policies, coordination needs, schema status and
configuration are all validated at boot, before a node reports ready.
Boot reports every problem it finds together, not just the first.

## Consequences

- A misconfigured node never takes traffic.
- Boot is slower and stricter. A prototype must still declare what it needs.
- A deploy fails fast and loudly instead of degrading quietly.

## Alternatives considered

- **Lazy validation on first use.** Faster start-up, but errors reach users.

## Sources

- [Part 5: principle 10](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#0-principles)
