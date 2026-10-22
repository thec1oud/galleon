# 0202. Cache authorization decisions per call only, and leave longer caching to the engine

Status: Proposed
Date: 2026-10-22

## Context

One call can ask the same authorization question several times: the
pipeline checks the action before decode, the handler checks it again
before an optional step, and a list asks about the same resource twice.
Asking the decider each time costs a policy evaluation, or a network round
trip for a remote engine. Caching a decision for longer than a call is
faster still, but a cached allow outlives a revoked role or a deleted
membership, and only the engine knows when its policies change.

## Decision

- Galleon caches each decision **for the rest of the call** that asked
  for it, keyed by its action and resource. The subject and context are
  fixed for a call, so they need no place in the key.
- A query whose resource carries **attributes** isn't cached, since two
  queries about one resource can differ in what they say about it. Nor is a
  decision that carries **obligations**, which must be fulfilled each time.
- Galleon never caches **across calls**. An engine that wants that, such
  as an embedded one evaluating a synced copy of its policies, does it
  behind the `Decider` seam, where it can invalidate correctly.
- `CanEach` asks the decider once, through `CheckMany`, for the queries
  not already cached.

## Consequences

- Handlers can check freely without paying for repeats, and a list can be
  checked in one batch.
- A change of policy takes effect from the next call, never midway through
  one that already decided.
- Apps with a remote decider pay one round trip per distinct question per
  call. Making that cheaper is the engine's job, not Galleon's.

## Alternatives considered

- **No caching.** Correct, but repeats every evaluation and leads handlers
  to avoid checking.
- **A cross-call cache in Galleon with a TTL.** Faster, but every TTL is
  a window in which a revoked permission still works, and Galleon can't
  know when an engine's policies change.
- **A TTL on each decision, set by the engine.** The seam sketch had one.
  Within a single call it buys nothing the per-call cache doesn't, so it
  was left out.

## Sources

- [Part 5, §8.3: decision caching is per request](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#83-authorization-decisions-stage-8)
