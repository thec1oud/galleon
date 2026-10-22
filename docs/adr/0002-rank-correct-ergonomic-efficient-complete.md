# 0002. Rank correctness first, then ergonomics, efficiency, and completeness in the core

Status: Accepted
Date: 2026-10-20

## Context

Galleon aims to be correct, complete, ergonomic and efficient. Those goals
collide constantly: typed extractors against a quick `c.Get("user")`,
refusing to boot on a route with no auth policy against a fast prototype,
shipping every feature against a core a small team can maintain. Without a
ranking, every such collision is argued from scratch.

## Decision

Galleon ranks its goals **correct > ergonomic > efficient > complete in the
core**.

Efficiency has a hard budget: no ergonomic feature may add per-request
reflection or allocation on the hot path unless the user opts in. Each
component is to document its hot-path cost, and benchmarks in CI are to
catch regressions.

Completeness is delivered through batteries and seams, not a bigger core.

## Consequences

- A correctness bug in a framework is copied into every app built on it, so
  correctness wins every tie.
- "Efficient" becomes reviewable: a change either adds hot-path cost or it
  doesn't.
- Most of the conflict between ergonomics and efficiency is resolved at boot
  (the DI graph, validation plans), or is to be resolved by generated code
  (generated decoders), not per request.
- The core stays small; a feature that isn't needed for correctness goes in a
  battery.

## Alternatives considered

- **Efficiency above ergonomics.** For most apps the database and network
  dominate latency. Fiber shows the cost: speed bought with compatibility and
  safety.
- **Completeness in the core.** Completeness is the easiest of the four to
  add later, and a large core is what sank Buffalo.

## Sources

- [Part 4: decision D5](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#decisions-so-far)
- [Part 4: ergonomics vs efficiency, in Round 2](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#ergonomics-vs-efficiency-d5-ergonomics-with-an-efficiency-budget)
- [Part 4: Pushback 7](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-7-correct-complete-ergonomic-will-conflict-so-rank-them)
- [Part 5: principle 1](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#0-principles)
