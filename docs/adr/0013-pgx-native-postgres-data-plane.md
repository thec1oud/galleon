# 0013. Make the data plane pgx-native and PostgreSQL-only for now

Status: Accepted
Date: 2026-10-20

## Context

The first-party ORM and the dpg migration tool are both PostgreSQL-only.
Galleon's data plane needs nested transactions (savepoints), pipelined
batches, `COPY`, and access to WAL positions for read-your-writes, none of
which `database/sql` exposes well. `database/sql` has no nested
transactions at all.

## Decision

Galleon's data plane targets **PostgreSQL only**, and its seam is
**pgx-native**: rows, batches and command tags are pgx's own types. The
default implementation is package `pg`, built on `pgxpool`. Support for
other databases, if it comes, goes through a `database/sql` adapter.

## Consequences

- The data plane can use savepoints, batching and WAL positions directly, and
  is faster than going through `database/sql`.
- Code written against pgx reads the same inside Galleon.
- Apps on MySQL, SQLite or other databases are not served by the data plane
  until an adapter exists.

## Alternatives considered

- **`database/sql` as the seam.** Portable, but it can't express nested
  transactions or WAL positions, so read-your-writes and the transaction
  rules in ADR 0014 would be impossible.

## Sources

- [Part 4: decisions D3 and D4, and the note in Round 2](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#round-2-my-responses-to-your-answers)
