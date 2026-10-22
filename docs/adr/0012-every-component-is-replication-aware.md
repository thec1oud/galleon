# 0012. Make every stateful component state whether its state is per process or cluster-wide

Status: Accepted
Date: 2026-10-20

## Context

Galleon apps are expected to run on several nodes. Components that hold
state, such as caches, rate limiters, schedulers and sessions, behave
differently on one node and on many, and a component that assumes one node
breaks silently on many.

## Decision

Every component that holds state states whether that state is per process or
cluster-wide. A component gets cluster-wide state only through the
coordination interface (ADR 0011), never through a side channel of its own.

## Consequences

- Multi-node behaviour is designed in, not discovered in production.
- Boot can check each component's needs against the configured backend.
- Each component has to document its replication behaviour.

## Alternatives considered

- **Per-component cluster support.** Each component picks its own mechanism,
  and the guarantees can't be checked or reasoned about together.

## Sources

- [Part 5: principle 8](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#0-principles)
