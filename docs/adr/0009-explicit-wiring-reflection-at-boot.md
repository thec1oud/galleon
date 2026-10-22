# 0009. Wire explicitly, and allow reflection at boot but never per request

Status: Accepted
Date: 2026-10-20

## Context

Frameworks often wire components through global registries, `init()`
side effects or per-request reflection. These are convenient, but they hide
where things come from, make tests share state, and add hot-path cost.
Galleon still needs dependency injection that is pleasant to use.

## Decision

- **No global state and no init-time registration.** Everything is
  constructed and passed in, and every piece of wiring can be found with
  "go to definition".
- **Dependency injection uses reflection at boot by default.** Code
  generation is optional, for teams that want it.
- **Reflection is allowed at boot, never per request** (ADR 0002's budget).

## Consequences

- Two apps, or two tests, can run in one process without interfering.
- Boot does the expensive work once; the hot path stays free of reflection.
- Every user avoids an extra build step for DI, while teams that want
  compile-time wiring can opt in.

## Alternatives considered

- **Code-generated DI by default.** No reflection at all, but a build step for
  everyone, for a cost that is paid once at boot anyway.
- **Global registries and `init()` registration.** Convenient, but hidden
  and hard to test.

## Sources

- [Part 5: principle 9](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#0-principles)
- [Part 6: §0, item 2](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md#0-open-items-from-part-5-settled-with-defaults)
