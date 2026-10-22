# 0201. Choose each operation's authenticators by scheme, and stop at invalid credentials

Status: Proposed
Date: 2026-10-22

## Context

An app often accepts more than one kind of credential: bearer tokens from
an identity provider for people, API keys for scripts, and later sessions
for a browser. Not every operation should accept every kind. A webhook
endpoint may take only a signed key; an admin operation may refuse API
keys. When several authenticators run, the order and the handling of a
failure decide whether a bad credential can be ignored in favour of a
weaker one, or quietly downgraded to anonymous.

## Decision

- An authenticator names one **scheme**, such as `bearer` or `apikey`. An
  operation lists the schemes it accepts with `Schemes(...)`; one that lists
  none accepts every scheme the app installed. Boot fails if an operation
  names a scheme no authenticator has.
- The operation's authenticators run **in order**. One that finds no
  credentials of its scheme returns `ErrNoCredentials`, and the next runs.
- Credentials that are present but don't verify **stop the chain** with
  `401`, on every operation, public ones included. No other scheme is tried
  after them.
- A request with no credentials any scheme accepts runs as the **anonymous
  principal**. Whether that is enough is the decision stage's call; an
  operation that isn't `Public()` is denied with `401`.
- An authenticator that fails for another reason, such as its key store
  being down, fails the call with `503`, never as anonymous.

## Consequences

- A bad token can't be shadowed by a weaker credential, or by none: a
  client always learns its credentials were refused.
- A public operation called with a broken token fails, where some frameworks
  would ignore the token. Clients must stop sending credentials they know
  are bad.
- Authentication and authorization stay separate: the chain says who is
  calling, and only the decider says what they may do.

## Alternatives considered

- **Every authenticator on every operation.** Simpler, but an operation
  couldn't refuse a kind of credential, and every scheme's cost would be
  paid on every call.
- **Try the next scheme after invalid credentials.** Lets a bad token fall
  through to a weaker scheme, or to anonymous, and hides the failure.
- **Ignore invalid credentials on public operations.** Convenient for
  clients, but the same request then behaves differently on two operations.

## Sources

- [Part 6, §7.1: the identity seam's rules](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md#71-identity)
- [Part 5, §8.1: a chain per operation](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/05-architecture.md#81-identity-stage-3)
