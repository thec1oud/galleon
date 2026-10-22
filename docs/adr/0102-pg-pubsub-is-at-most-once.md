# 0102. Keep coord-pg's pub/sub at-most-once over LISTEN/NOTIFY, with a channel per topic

Status: Proposed
Date: 2026-10-22

## Context

The coordination seam's pub/sub is at-most-once, with no order across
publishers (rule 4); anything that must not be lost uses the outbox.
`coord-pg` needs pub/sub on PostgreSQL alone. `LISTEN/NOTIFY` delivers to
the sessions listening when a transaction commits, holds nothing for a
session that isn't connected, and takes payloads under 8000 bytes.

The same backend also needs to wake waiters for locks, leader observers and
key-value watchers. Those must not miss a change.

## Decision

`coord-pg` publishes with `pg_notify`, one channel per topic, named by the
topic's hash under the store's schema, with the topic in the payload so two
topics that share a hash are told apart. A node listens, on one connection
of its own, only to the channels of topics it subscribes to. `Subscribe`
returns once the node listens. A publish whose topic and base64 data don't
fit the payload limit fails with `coord_pg.message_too_large`; nothing is
dropped silently at publish. A slow subscriber drops messages past a buffer
of 256.

Notifications only ever wake the parts that must not miss a change. Lock
waiters and leader observers read the lease row again, and key-value
watchers read a history table, so a lost notification costs latency, not
correctness. When the listening connection drops, the backend reconnects,
listens again and wakes every waiter and watcher.

## Consequences

- No extra infrastructure, and a node receives only the topics it
  subscribes to.
- Messages published while a node's listener is down are lost to it, as
  at-most-once allows.
- Data is limited to about 5.9KB per message.
- Key-value watches survive reconnects, because they never depended on
  delivery.

## Alternatives considered

- **One channel for every topic.** Simpler, but every node receives every
  message.
- **A message table, read by subscribers.** At-least-once within a window,
  more than the seam promises, at the cost of writes and pruning for every
  message. The outbox already serves that need.
- **Payloads stored in a table, with only an ID notified.** Lifts the size
  limit, at the cost of a write per message; left until a need appears.

## Sources

- [Part 6: §4, coordination rule 4](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md)
- [PostgreSQL: NOTIFY](https://www.postgresql.org/docs/current/sql-notify.html)
