# 0022. Ship the frontend as a battery: templ, fragment responses, and Vite as a seam

Status: Accepted
Date: 2026-10-20

## Context

Galleon must support full-stack applications. Owning a JavaScript toolchain
is what sank Buffalo, and the standard library's `html/template` has no
component model and no type safety.

## Decision

The frontend will be an optional **battery** (`web`) with a seam. None of it
is built yet. Its defaults are to be:

- **templ** for type-safe, compiled HTML components.
- **Fragment responses**, htmx and Hotwire style: partial rendering,
  out-of-band updates, and fragment-request headers, without shipping a JS
  framework.
- **Vite as the asset seam**, not an owned toolchain: Galleon will read
  Vite's manifest for fingerprinted URLs, proxy its dev server, and embed the
  built output in the binary.

SPAs and meta-frameworks will use the same Vite seam, and `html/template`
will stay supported as an alternative. A server-driven, LiveView-style UI can come
later as its own battery, after the realtime work.

## Consequences

- Full-stack apps will get type-checked templates and interactivity without an
  SPA by default.
- Galleon never owns the JS build, so it can't break when that ecosystem
  moves.

## Alternatives considered

- **`html/template` by default.** No dependency, but no components or type
  safety.
- **An owned JS toolchain.** The Buffalo lesson.

## Sources

- [Part 4: decision D6 and Round 2](https://github.com/thec1oud/galleon/blob/0897d74/docs/design/04-desires-debate.md#frontend-default-d6-my-suggestion)
