# 0203. Prepare each module's pipeline stage once per operation, at boot

Status: Proposed
Date: 2026-10-22

## Context

A module that implements a pipeline stage, such as authorization, used to
install one interceptor for every operation. Much of what that interceptor
does depends only on the operation's descriptor: which authenticators its
schemes name, where its input keeps the resource's ID, whether it needs the
stage at all. Working that out per call wastes time on every request, and
finding a mistake per call means the first caller finds it, not boot.

## Decision

- The kernel offers `Registry.PrepareStage(stage, prepare)`. While `New`
  compiles the pipelines, it calls `prepare` with each operation's
  descriptor. `prepare` returns that operation's interceptor for the stage,
  or nil to leave the stage out of its chain, or an error that fails boot
  naming the operation.
- A stage the operation's security declaration needs can't be left out: a
  nil interceptor for it fails boot too.
- `ImplementStage` stays, as the case where every operation gets the same
  interceptor.

## Consequences

- A declaration mistake, such as a scheme no authenticator has or a
  resource field the input lacks, fails boot with the operation's name and
  site.
- Operations that don't need a stage don't run it: a public operation skips
  authorization entirely.
- A stage's implementer can't change its mind per call about which
  operations it serves; the choice is made at boot.

## Alternatives considered

- **Per-call preparation, cached on first use.** Reports mistakes to the
  first caller and adds a lookup to every call.
- **Pass the descriptor to `ImplementStage`'s interceptor at compile time
  without allowing an error.** Prepares per operation but can't fail boot,
  so mistakes would still surface per call.

## Sources

- [ADR 0010: validate everything at boot](0010-fail-at-boot.md)
- [Part 6, §5.1, rule 5: the chain is prepared at boot](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/06-seam-specifications.md#51-descriptor-and-invocation)
