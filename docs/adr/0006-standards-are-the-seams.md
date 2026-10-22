# 0006. Where an industry standard exists, use it as the seam

Status: Accepted
Date: 2026-10-20

## Context

Galleon wants the most standard seam possible for every component it doesn't
bundle. For many areas, an industry standard already defines the boundary,
and tools already implement it.

## Decision

Where a standard exists, the standard is the seam, and Galleon does not wrap
it in an abstraction of its own:

| Area | Standard |
|---|---|
| HTTP handlers | `net/http` |
| SQL | `pgx` (and `database/sql` for other databases) |
| Observability | OpenTelemetry |
| Logging | `log/slog` |
| Feature flags | OpenFeature |
| Authentication | OAuth 2.1 / OIDC, JWT, WebAuthn |
| Authorization decisions | OpenID AuthZEN |
| User provisioning | SCIM 2.0 |
| Events and the outbox envelope | CloudEvents |
| REST, RPC and event contracts | OpenAPI 3.1 and JSON Schema; Protobuf, gRPC and Connect; AsyncAPI |
| Errors | RFC 9457 problem+json |
| Health checks | Kubernetes probe conventions, the gRPC health protocol |
| Rate limit headers | IETF `RateLimit` headers |

## Consequences

- Tools that implement the standard work with Galleon without a
  Galleon-specific adapter.
- Galleon inherits each standard's limits and its pace of change.

## Alternatives considered

- **A Galleon abstraction over each standard.** It adds a layer users must
  learn and adapters must target, for no gain where the standard already is
  the abstraction.

## Sources

- [Part 4: where "most standard seam" means "adopt the existing standard"](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#where-most-standard-seam-means-adopt-the-existing-standard)
- [Part 5: principle 5](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#0-principles)
