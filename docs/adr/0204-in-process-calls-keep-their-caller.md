# 0204. Run calls made in process as their caller, and never trust a caller from a network call's context

Status: Proposed
Date: 2026-10-22

## Context

Code calls operations in process: a handler calls another operation, a
test calls one directly, and jobs will run them. Such a call carries no
request headers, so authenticating it would always produce the anonymous
principal, and every protected operation would refuse it. But if a
principal could be carried in a `context.Context` into any call, a value
set by middleware or a bug could stand in for real credentials on a call
that arrived over the network.

## Decision

- A call made in process, whose request's transport is `local` (as
  `Operations.Call` makes it) or `job`, runs as the principal of the call it
  was made from, or the one set with `security.WithPrincipal`. It is still
  authorized as that principal at every stage.
- A call that arrives over a network, over HTTP or RPC, **always**
  authenticates its own request. A principal in its context is ignored.
- Calls made in process aren't subject to an operation's `Schemes`, which
  restrict how a request presents credentials, not who may call.

## Consequences

- Handlers can compose operations, and tests can call protected operations
  as a chosen user, without forging headers.
- No client can become another principal through a context value: the only
  way in from the network is credentials an authenticator verifies.
- Code that calls in process must choose the principal deliberately; there
  is no implicit system user.

## Alternatives considered

- **Authenticate every call, in process or not.** Calls made in process
  would always be anonymous, so composing operations would need forged
  headers.
- **Trust a principal in any call's context.** Convenient, but any outer
  middleware could then bypass authentication for network calls.
- **A built-in "system" principal for calls made in process.** Hides who
  really asked, and tends to be allowed everything.

## Sources

- [Part 5, §6.1: everything request-scoped is reached through typed accessors on ctx](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#61-the-operation-model-p10-i1)
