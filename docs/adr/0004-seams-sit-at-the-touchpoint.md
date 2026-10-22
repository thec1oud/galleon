# 0004. Put a seam at the touchpoint, not over the whole tool

Status: Accepted
Date: 2026-10-20

## Context

Galleon must let people plug in their own tools: an ORM, an IAM system, a
migration tool, billing, messaging. The most common mistake in frameworks
with pluggable parts is a seam that wraps an entire category, such as a
generic "ORM interface". It ends up as the lowest common denominator of every
tool behind it, ergonomic for none of them.

## Decision

A seam captures only **what Galleon itself needs to touch**, never the full
API of the tool behind it. Users talk to their ORM, IAM or billing system
through that tool's own API. Galleon defines only the narrow points where its
lifecycle, request pipeline or coordination layer meets the tool.

For example, the ORM seam is a connection source and a transaction carried in
`context` that other Galleon parts can join, plus health checks and tracing
hooks. Queries, models and relations stay the ORM's own API.

## Consequences

- Seams stay small enough to implement fully and to test with a conformance
  suite.
- Users keep the full power of their chosen tool.
- Galleon can't offer one API that works across every ORM or IAM. That is
  deliberate.

## Alternatives considered

- **Category-wide abstractions.** They force every tool down to the common
  subset and make Galleon responsible for an API it doesn't need.

## Sources

- [Part 4: Pushback 2](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-2-seams-must-sit-at-the-touchpoint-not-wrap-the-whole-tool)
- [Part 5: principle 3](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#0-principles)
