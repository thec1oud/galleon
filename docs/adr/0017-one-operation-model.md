# 0017. Describe every API operation with one runtime model, whatever authored it

Status: Accepted
Date: 2026-10-20

## Context

Operations can be authored in Themis, the first-party API DSL, or by hand in
Go. Routing, contract export, docs, authorization, client generation and
contract diffing all need the same facts about each operation. If each of
those hung off one authoring format, Galleon would depend on it, and
switching formats would mean rewriting handlers.

## Decision

Every operation becomes an **operation descriptor** inside Galleon: one
transport-neutral runtime description holding its identity, transport
bindings, input and output types with their decoders, encoders and
validators, its errors, its security requirements, its policies (timeouts,
limits, idempotency, caching), its streaming mode and its docs metadata.

The descriptor is Galleon's own (ADR 0007). **How it is authored is a seam**:
Go code builds descriptors with generics today, and Themis will generate
them. Galleon is to publish a small, versioned target package that
generators implement, and Galleon never depends on Themis.

## Consequences

- One source of truth drives routing and authorization now, and is to drive
  OpenAPI, proto and AsyncAPI export, docs and clients.
- People can leave Themis without rewriting handlers.
- A Themis-generated operation and the same operation written by hand must
  pass the same conformance suite.
- The target package falls under Galleon's compatibility promise.

## Alternatives considered

- **Themis as the only authoring format.** Simpler, but couples Galleon to a
  DSL and locks users in.
- **Plain Go only.** Gives up generated, reflection-free decoders and
  multi-language clients.

## Sources

- [Part 4: decision D9 and Pushback 10](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#pushback-10-themis-the-api-dsl)
