# 0016. Declare read consistency per read, with typed handles and signed causal tokens

Status: Accepted
Date: 2026-10-20

## Context

Apps with read replicas need reads that are fast but not wrong. The prior
art guesses: Rails sends a user's reads to the primary for a fixed window
after a write, and Laravel stays sticky only within one request. PostgreSQL
can answer exactly whether a replica has replayed a given WAL position.

## Decision

- **Declare intent, never pick a connection.** A module states what a read
  needs, and Galleon picks the source.
- **Read and write handles are different types.** A read handle has no write
  methods, so writing through it doesn't compile, and read transactions run
  `READ ONLY` as a backstop.
- **A consistency level per read:** `Primary`, `Any`,
  `BoundedStaleness(d)`, and `ReadYourWrites`. A snapshot shared between
  transactions is planned.
- **Read-your-writes uses causal tokens.** After a write commits, Galleon
  captures its WAL position and returns an opaque, signed, versioned token in
  the `Galleon-Consistency` response header (or RPC metadata). A later read
  that carries it goes to a replica that has replayed past that position, or
  to the primary. The token's position is clamped to the primary's, so a
  forged token can at worst force a primary read.

## Consequences

- Read-your-writes holds across requests, across nodes, and for mobile
  clients that echo the header, and the primary is used only when no replica
  has caught up.
- Capturing the WAL position adds one statement to a write's commit, which
  can be pipelined with it.
- Clients must echo the token to get read-your-writes across requests.

## Alternatives considered

- **Time-window stickiness (Rails).** A guess about replication lag that is
  wrong under load and covers only one user.
- **Per-request stickiness (Laravel).** Breaks on the next request or another
  node.
- **Routing in a proxy.** The proxy can't see whether a read must be fresh.

## Sources

- [Part 4: Pushback 9](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-9-read-and-write-transaction-mixes)
- [Part 4: causal tokens, what's standard and what they cost](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#causal-tokens-question-9-whats-standard-and-what-they-cost)
- [Part 6: §0, item 5](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md#0-open-items-from-part-5-settled-with-defaults)
