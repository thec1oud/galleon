# 0015. Let a read inside a write transaction join it by default

Status: Accepted
Date: 2026-10-20

## Context

When a module asks for a read while someone else's write transaction is
open, the read can either reuse that transaction or open a separate one,
possibly on a replica. Silently sending it to a replica is a classic bug:
the read can't see the enclosing uncommitted writes, and it may be stale.
Spring's propagation rules are the best-known vocabulary for this.

## Decision

A transaction **joins** the enclosing one by default, so a read inside a
write sees its uncommitted writes. A module that wants a separate
transaction asks for it with **Independent**. Galleon also offers **Nested**
(a savepoint) and **Mandatory** / **Never** (assert that a transaction is or
isn't present). In development, an Independent read inside an open write
transaction is flagged.

## Consequences

- The default is the one that can't read stale or inconsistent data.
- Offloading reads to replicas inside a write takes an explicit choice.

## Alternatives considered

- **Independent by default.** Spreads load, but produces reads that miss the
  enclosing writes, silently.

## Sources

- [Part 4: decision D8 and Pushback 9, point 5](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-9-read-and-write-transaction-mixes)
