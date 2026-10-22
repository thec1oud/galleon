# 0014. Offer both begin/commit pairs and closures for transactions, over one implementation

Status: Accepted
Date: 2026-10-20

## Context

Closures (`Write(ctx, func(tx) error)`) are safe and can retry, but a
transaction's lifetime isn't always lexical: a request-scoped transaction
opened in middleware, a streaming response, or a multi-step handler needs
begin/commit pairs. Pairs are easy to misuse without rules.

## Decision

Galleon offers both forms, and **the closure is sugar over the pair**, so
there is one implementation. Three rules keep pairs correct:

1. **Ownership.** Only the code that began the root transaction can commit
   it. `Begin` on a handle you were given creates a savepoint, so a callee's
   `Commit` can never commit its caller's work. The type enforces this.
2. **`defer tx.Rollback(ctx)` is always safe**: a no-op after `Commit`.
3. **Leak detection.** A transaction still open when its request or job ends
   is reported with where it began, and rolled back.

Only the closure form **retries automatically** on serialization failures
and deadlocks (SQLSTATE `40001` and `40P01`), because only a closure can be
re-run. Joined or nested transactions never retry; the owner of the root does.

## Consequences

- Non-lexical transactions are possible without giving up safety.
- Retry is explicit: code using pairs must opt in to a retry helper, and the
  docs must say so.

## Alternatives considered

- **Closures only.** Can't express request-scoped or streaming transactions.
- **Pairs only.** Can't retry automatically, and is easier to misuse.

## Sources

- [Part 4: decision D10 and Round 2](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#round-2-my-responses-to-your-answers)
- [Part 4: Pushback 9, point 7](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-9-read-and-write-transaction-mixes)
