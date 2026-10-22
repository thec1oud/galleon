# 0001. Record architecture decisions in an append-only log

Status: Accepted
Date: 2026-10-22

## Context

Galleon's design was worked out in long discussions, and the documents in
`docs/design` record those discussions round by round. A reader can't tell
from them what is settled, what was overturned later, or why. The same
documents were also edited as the code was built, so the reasoning behind
an early decision is mixed with the description of what exists now.

## Decision

We keep one Architecture Decision Record (ADR) per significant decision in
`docs/adr`, numbered in order, following the format in
[template.md](template.md).

An ADR is append-only:

- While it is **Proposed**, it can be edited freely.
- Once it is **Accepted** (or **Rejected**), only its `Status:` line may
  change. CI enforces this.
- To change a decision, write a new ADR that supersedes the old one, and
  set the old one's status to `Superseded by [NNNN](NNNN-slug.md)`.
- An ADR that no longer applies, with nothing replacing it, becomes
  **Deprecated**.

ADRs record why. The rest of the documentation describes what is true now
and links to the ADRs behind it, instead of repeating the debate.

## Consequences

- The reasoning behind every decision survives, including the options we
  rejected, even after the decision is reversed.
- Current-state docs stay short, because they don't carry history.
- Changing a decision costs a new record, which is the point: it makes
  reversals visible and deliberate.
- A typo in an accepted ADR stays. That is an accepted cost.

## Alternatives considered

- **Keep decisions in the design documents.** That is what we had: settled
  and open questions mix, and edits erase the history.
- **Editable decision records.** Simpler, but a record that can be rewritten
  can no longer be trusted to say what we knew at the time.
- **Decisions in issues or PRs only.** They are hard to find and are not
  versioned with the code.

## Sources

- Michael Nygard, [Documenting architecture decisions](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions)
- [adr.github.io](https://adr.github.io/)
