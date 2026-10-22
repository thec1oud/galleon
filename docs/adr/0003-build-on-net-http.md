# 0003. Build on net/http and never replace it

Status: Accepted
Date: 2026-10-20

## Context

Go web frameworks either build on `net/http` (Chi, Echo, Gin) or replace it
with their own server (Fiber on fasthttp). Replacing it buys some speed and
costs compatibility with the entire ecosystem of `http.Handler` middleware,
HTTP/2, and tooling.

## Decision

`net/http` is Galleon's foundation. Everything Galleon serves mounts as an
`http.Handler`, and Galleon's handler and middleware model is built on top of
it rather than beside it.

## Consequences

- Any standard Go middleware and any `http.Handler` works with Galleon.
- Galleon gets HTTP/2, TLS and the standard library's security fixes for free.
- Galleon can't use a faster non-standard server. Under ADR 0002 that
  trade is acceptable.

## Alternatives considered

- **fasthttp or a custom server.** Faster on microbenchmarks, but
  incompatible with the ecosystem, and the database usually dominates real
  latency anyway.

## Sources

- [Part 5: principle 2](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#0-principles)
- [Part 4: Pushback 8 and the standards table](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-8-what-should-never-be-a-seam)
