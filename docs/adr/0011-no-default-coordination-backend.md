# 0011. Require an explicit coordination backend, with no default

Status: Accepted
Date: 2026-10-20

## Context

Galleon's cluster behaviour (locks, leader election, pub/sub, shared state)
goes through a coordination backend. A default backend would be convenient,
but an in-process default silently gives per-process guarantees to an app
that is actually running on several nodes, and a networked default forces
infrastructure on single-node apps.

## Decision

Galleon has **no default coordination backend**. Every app configures one
explicitly, including `local` for single-node deployments.

- Components declare what they need from coordination, and boot checks those
  needs against the configured backend.
- With `local`, components that need cluster-wide guarantees still work, but
  Galleon logs that their guarantees are per process, at boot and in the
  operations endpoint.
- An app declares its deployment. With `Deploy(MultiNode)`, a
  process-scoped backend fails boot unless the app explicitly accepts
  per-process guarantees.

Planned backends are `local`, Postgres, Redis, NATS and etcd, none of them
the default.

## Consequences

- No app gets per-process guarantees without knowing it.
- Every app needs one extra line of configuration.

## Alternatives considered

- **`local` by default.** Convenient, and wrong as soon as a second replica
  starts.
- **Postgres by default.** Forces a database on apps that don't need one for
  coordination.

## Sources

- [Part 4: decision D7 and its safeguard](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#coordination-backend-with-no-default-d7-agreed-plus-one-safeguard)
- [Part 6: §0, item 4](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md#0-open-items-from-part-5-settled-with-defaults)
