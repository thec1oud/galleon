# 0103. Let a coordination backend own its config and lifecycle through the app

Status: Proposed
Date: 2026-10-22

## Context

`galleon.Coordination` takes a backend value before the app exists, and
until now the only backend, `local`, needed nothing more. A cluster backend
holds resources: a pool, a listening connection, heartbeats. It needs
config an operator sets (a DSN, from the environment in production), it
must be running before any provider uses it in its own `Start`, and it must
stop after every provider has released its leases.

## Decision

The kernel treats a backend given to `galleon.Coordination` as a part of
the app when it implements the kernel's own interfaces:

- a backend that is a `galleon.Module` is installed first, before the
  modules given to `galleon.Modules`, so it reads its own config section;
- one that is a `galleon.Starter` starts before every provider, and a start
  that fails fails the app's;
- one that is a `galleon.Stopper` stops after every provider and stop hook.

Installing the same backend in `galleon.Modules` as well fails boot with a
message saying why.

## Consequences

- One line, `galleon.Coordination(coordpg.New())`, configures, starts and
  stops a cluster backend, and its config is checked at boot with the rest.
- Providers can take leases in `Start` and release them in `Stop`.
- The backend starts before the app's providers, so it can't take the
  app's logger from injection; it takes one as an option.
- A backend used outside an app, as in tests, is started and stopped by its
  caller.

## Alternatives considered

- **The app passes a backend module separately.** Two lines that must
  agree, and a backend that silently never starts if one is left out.
- **A backend that connects lazily on first use.** Hides a missing
  database until the first lock, after the app reports ready, against
  [0010](0010-fail-at-boot.md).

## Sources

- [0010: validate everything at boot](0010-fail-at-boot.md)
- [0011: no default coordination backend](0011-no-default-coordination-backend.md)
