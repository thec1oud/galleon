# Architecture decision records

Each record here captures one decision about Galleon's architecture: the
context, what we decided, the consequences, and the options we rejected.
Together they answer "why is it like this?". The rest of the documentation
describes what is true now and links here for the reasoning.

Records are **append-only**. Once a record is accepted, only its `Status:`
line may change. To change a decision, write a new record that supersedes
the old one. [ADR 0001](0001-record-architecture-decisions.md) explains why.

## Index

| # | Decision | Status |
|---|---|---|
| 0001 | [Record architecture decisions in an append-only log](0001-record-architecture-decisions.md) | Accepted |
| 0002 | [Rank correctness first, then ergonomics, efficiency, and completeness in the core](0002-rank-correct-ergonomic-efficient-complete.md) | Accepted |
| 0003 | [Build on net/http and never replace it](0003-build-on-net-http.md) | Accepted |
| 0004 | [Put a seam at the touchpoint, not over the whole tool](0004-seams-sit-at-the-touchpoint.md) | Accepted |
| 0005 | [Ship every seam with a default adapter, a conformance suite and a fake](0005-a-seam-ships-complete.md) | Accepted |
| 0006 | [Where an industry standard exists, use it as the seam](0006-standards-are-the-seams.md) | Accepted |
| 0007 | [Keep the parts that must never fork in Galleon, not behind seams](0007-galleon-owns-what-must-never-fork.md) | Accepted |
| 0008 | [Keep tools independent of Galleon, and put adapters in their own modules](0008-dependency-direction.md) | Accepted |
| 0009 | [Wire explicitly, and allow reflection at boot but never per request](0009-explicit-wiring-reflection-at-boot.md) | Accepted |
| 0010 | [Validate everything at boot, before a node reports ready](0010-fail-at-boot.md) | Accepted |
| 0011 | [Require an explicit coordination backend, with no default](0011-no-default-coordination-backend.md) | Accepted |
| 0012 | [Make every stateful component state whether its state is per process or cluster-wide](0012-every-component-is-replication-aware.md) | Accepted |
| 0013 | [Make the data plane pgx-native and PostgreSQL-only for now](0013-pgx-native-postgres-data-plane.md) | Accepted |
| 0014 | [Offer both begin/commit pairs and closures for transactions, over one implementation](0014-transaction-pairs-and-closures.md) | Accepted |
| 0015 | [Let a read inside a write transaction join it by default](0015-reads-join-the-enclosing-transaction.md) | Accepted |
| 0016 | [Declare read consistency per read, with typed handles and signed causal tokens](0016-typed-handles-and-causal-tokens.md) | Accepted |
| 0017 | [Describe every API operation with one runtime model, whatever authored it](0017-one-operation-model.md) | Accepted |
| 0018 | [Share one validation seam across layers, in two phases](0018-shared-validation-seam.md) | Accepted |
| 0019 | [Define a tool-neutral migration seam: a small core plus optional capabilities](0019-tool-neutral-migration-seam.md) | Accepted |
| 0020 | [Integrate every external service through one kit, with CloudEvents on the wire](0020-one-integration-kit.md) | Accepted |
| 0021 | [Keep authorization generic: resolve context first, then decide locally](0021-generic-authorization-seams.md) | Accepted |
| 0022 | [Ship the frontend as a battery: templ, fragment responses, and Vite as a seam](0022-frontend-battery.md) | Accepted |

Records 0002 to 0022 were written on 2026-10-22 from the design discussion
that preceded them. Their dates are when each decision was first committed
to the repository, and their Sources sections link to that discussion at
commit 0897d74, its state when these records were written. Where a record
describes something not built yet, it says "will" or "is to".

## Writing a record

1. Copy [template.md](template.md) to `NNNN-short-slug.md`, using the next
   free number. The slug is lowercase and hyphenated.
2. Fill it in with `Status: Proposed` and today's date, and add it to the
   index above.
3. Open a PR. Discuss and edit the record freely while it is proposed. CI
   marks the PR as failing while any record is still Proposed, so it can't
   merge undecided.
4. Before merging, set `Status: Accepted` (or `Rejected`, which is worth
   keeping too: it records an option we chose not to take), in the record
   and in the index.

Write a record when a decision is hard to reverse, affects more than one
module, or settles a debate that would otherwise come up again. Ship it in
the same PR as the code and docs it affects.

## Changing a decision

Write a new record that explains what changed and why. In the same PR, set
the old record's status line to `Status: Superseded by [NNNN](NNNN-slug.md)`
pointing at the new one, and update its status in the index. If a decision
simply no longer applies, set it to `Status: Deprecated` instead.

Nothing else in an accepted record changes, not even a typo. CI enforces
this with [`.github/scripts/adr-check.sh`](../../.github/scripts/adr-check.sh),
which also checks every record's name, header and sections, that a
superseding record exists and is accepted, and that the index has exactly
one up-to-date row per record. Run it locally from the repository root
with `.github/scripts/adr-check.sh --decided "$(git merge-base origin/dev HEAD)"`
(it needs bash 4.4 or later).

| Status | Meaning | Can change to |
|---|---|---|
| Proposed | Under discussion; edit freely | Anything |
| Accepted | In force | Superseded by, Deprecated |
| Rejected | Considered and declined | Nothing |
| Superseded by NNNN | Replaced by a later record | Nothing (it keeps pointing at its successor, even once that is superseded in turn) |
| Deprecated | No longer applies, with no replacement | Nothing |
