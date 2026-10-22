# 0020. Integrate every external service through one kit, with CloudEvents on the wire

Status: Accepted
Date: 2026-10-20

## Context

comms, vantage and billing each need the same things: reliable outbound
events, exactly-once-in-effect inbound events, a resilient typed client, a
local cache of what the service knows so the hot path makes no network call,
and signed webhooks or a stream. Three bespoke seams would solve the same
problems three times, and every third-party service would need a fourth.

## Decision

Galleon's core will have one **integration kit**: outbox, inbox and
deduplication, event consumer, resilient typed client, a local cache with
invalidation built on coordination, and webhook verification. comms,
vantage, billing and any third party (Stripe, Auth0, Twilio) will be thin
adapters on it. **CloudEvents** is the envelope for everything passing
through the outbox and inbox.

## Consequences

- Integrating an external service will be one uniform, first-class thing.
- Outbox writes will commit atomically with business data through the
  transaction seam.
- The kit is a large piece of core work that several features wait on.

## Alternatives considered

- **A seam per service.** Duplicates the same delivery guarantees in each.

## Sources

- [Part 4: Pushback 3](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-3-your-services-all-integrate-the-same-way-so-galleon-should-own-that-pattern)
