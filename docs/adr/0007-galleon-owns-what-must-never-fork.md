# 0007. Keep the parts that must never fork in Galleon, not behind seams

Status: Accepted
Date: 2026-10-20

## Context

Almost everything in Galleon can be a battery or a seam. But some parts, if
made pluggable, would split the ecosystem into incompatible halves or make
correctness impossible to prove, because every other component depends on
their exact behaviour.

## Decision

Galleon owns these, and they are never seams:

- the handler and middleware model, on top of `http.Handler` (ADR 0003)
- the **operation model**: the runtime description of every API operation
  (ADR 0017). How operations are *authored* is a seam.
- the error model
- context propagation and the transaction handle
- the coordination interface. Its backends are pluggable; the interface is
  Galleon's.
- the lifecycle and process model
- observability, which is OpenTelemetry directly rather than a Galleon
  abstraction over it (ADR 0006)

## Consequences

- Batteries, adapters and apps can rely on one behaviour for each of these.
- Changing any of them is a breaking change for the whole ecosystem, so they
  fall under Galleon's compatibility promise.

## Alternatives considered

- **Make these pluggable too.** Each would fork the ecosystem: two error
  models, two transaction handles, middleware that works with only one.

## Sources

- [Part 4: Pushback 8](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-8-what-should-never-be-a-seam), revised by [Pushback 10](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-10-themis-the-api-dsl)
- [Part 5: principle 6](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#0-principles)
