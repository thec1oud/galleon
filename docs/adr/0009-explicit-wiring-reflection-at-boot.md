# 0009. Wire explicitly, and allow reflection at boot but never per request

Status: Accepted
Date: 2026-10-20

## Context

Frameworks often wire components through global registries, `init()`
side effects or per-request reflection. These are convenient, but they hide
where things come from, make tests share state, and add hot-path cost.
Galleon still needs dependency injection that is pleasant to use.

## Decision

- **No global registries and no init-time registration.** Components are
  constructed and passed in, and every piece of wiring can be found with
  "go to definition". The one exception is the process's own identity under
  a supervisor, which package `process` reads from its environment once at
  startup.
- **Dependency injection uses reflection at boot by default.** Code
  generation is to be an option for teams that want it.
- **Reflection is allowed at boot, never per request** (ADR 0002's budget).

## Consequences

- Two apps, or two tests, can run in one process without interfering.
- Boot does the expensive work once; the hot path stays free of reflection.
- No user needs an extra build step for DI. Teams that want compile-time
  wiring will be able to opt in once code generation exists.

## Alternatives considered

- **Code-generated DI by default.** No reflection at all, but a build step for
  everyone, for a cost that is paid once at boot anyway.
- **Global registries and `init()` registration.** Convenient, but hidden
  and hard to test.

## Sources

- [Part 5: principle 9](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#0-principles)
- [Part 6: §0, item 2](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md#0-open-items-from-part-5-settled-with-defaults)
