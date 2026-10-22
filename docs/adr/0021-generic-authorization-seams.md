# 0021. Keep authorization generic: resolve context first, then decide locally

Status: Accepted
Date: 2026-10-20

## Context

vantage, the first-party IAM, introduces context-based access control
(CBAC), which resolves which operating context a person is in (user, org,
team, locality) before RBAC/ABAC runs. CBAC is still in active development.
Authorization is checked on almost every request, often several times, so a
remote call per check would add latency and make every app depend on the IAM
being up. Validation errors can also leak an input's shape to a caller who
isn't allowed to call the operation at all.

## Decision

- **Galleon doesn't depend on vantage's shape.** Context resolution and
  authorization are generic seams any IAM can fill; vantage is one adapter.
- A **context-resolution stage** will run after authentication and before
  the handler, producing a typed operating context. Operations declare the
  contexts they accept, so the gate is a cheap comparison.
- **Decisions are made locally**: an IAM like vantage is the source of truth,
  and an embedded engine will evaluate decisions against a synced copy of its
  policies. Galleon will supply **context providers** that assemble the
  request context a decision needs; billing entitlements can be one of them.
- **Authorization is split in two**: the action check runs before decode, and
  the resource check runs after decode, when the input is known. Every
  operation must declare its security (public, an action, or an action and
  resource), or boot fails.
- Standards at this seam: OIDC for authentication, OpenID AuthZEN for
  decisions, SCIM for provisioning.

## Consequences

- Any IAM can plug in, and vantage can change without breaking Galleon.
- A caller without permission gets `403` before learning anything from
  validation errors.
- The CBAC-specific parts of the seam are revisited when vantage's CBAC spec
  is stable.

## Alternatives considered

- **Model the seam on vantage.** Couples Galleon to an IAM still in design.
- **A remote call per decision.** Adds latency and makes the IAM a single
  point of failure.
- **One authorization check after decode.** Leaks input shape to callers who
  may not call the operation.

## Sources

- [Part 4: decisions D1, D2 and D11, Pushbacks 4 and 5](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-4-vantage-as-a-pure-service-would-put-a-network-call-on-every-request)
- [Part 6: §0, items 1 and 3](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md#0-open-items-from-part-5-settled-with-defaults)
