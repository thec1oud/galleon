# 0101. Keep coord-pg's leases as rows judged on the database's clock, fenced by one durable epoch

Status: Proposed
Date: 2026-10-22

## Context

`coord-pg` is the first cluster coordination backend. A lease must give one
holder at a time as judged by fencing token, with tokens that strictly
increase per key across restarts of the backend (rule 2 of the coordination
seam), and `Lost()` must close before another node can take the key, or the
backend must say what it guarantees instead (rule 3). Leases will be held in
large numbers once jobs (phase 6) take one per running job.

The design (05 §4.3) named PostgreSQL advisory locks for leases. An
advisory lock lives on a session: every held lease pins one connection, and
a holder that is frozen or cut off keeps the key until the server notices
its connection is dead, which with default TCP keepalives can take hours.
The TTL a caller asks for would not be enforced by anything.

## Decision

A `coord-pg` lease is a row of a leases table: the key, the holder's random
ID, the fencing token of its last grant, and its expiry on the database's
clock. One statement grants a free or expired key and adds one to its token.
The row is never deleted, so a key's token never goes back while the schema
lives.

The store has **one epoch**, made at random when the schema is created and
kept in it. Every node and every restart reports it, so their tokens
compare. The epoch changes only if the schema is dropped and created again,
and then a fenced resource sees a new epoch take over, as it does for the
in-process backend.

The holder measures its deadline from just before the statement that
granted or renewed the lease, which falls before the row's expiry. A lease
that isn't renewed is released shortly before that deadline (a tenth of its
TTL, at most 100ms), and `Lost()` closes once the release returns, never
after the deadline. That is rule 3's first guarantee for a holder whose
process keeps running; for one that is paused, the fencing token is the
guarantee.

Advisory locks are kept for one thing: transaction-level, to let one node
create the schema while others starting at the same time wait.

## Consequences

- Leases use the shared pool. Holding a thousand leases costs a thousand
  rows, not a thousand connections.
- The TTL is enforced on the database's clock, so a frozen or partitioned
  holder can't block a key past it, and nodes' clocks needn't agree.
- A crashed holder's key frees when its TTL runs out, not the moment its
  connection drops. Failover takes up to the TTL, which callers choose.
- A lease ends slightly before its full TTL when it isn't renewed, so
  holders should renew well before the end, as the migration runner does at
  every quarter.
- Waiting for a key is woken by `NOTIFY` and by the holder's expiry, with a
  bounded poll in case a notification is lost.

## Alternatives considered

- **Session advisory locks.** Free on crash, but one connection per lease,
  and no enforced TTL for a frozen or partitioned holder.
- **Advisory locks plus a token row.** Keeps both costs of advisory locks,
  and still needs the row for the token.
- **A global sequence for tokens.** Also increasing per key, but a token
  row per key needs no extra object and keeps the key's history in one
  place.

## Sources

- [Part 5: §4.3, backends](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md)
- [Part 6: §4, coordination rules 1 to 3](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md)
- The pull request that adds `coord-pg`, where table leases were chosen over advisory locks.
