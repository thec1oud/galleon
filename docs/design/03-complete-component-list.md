# Galleon, Part 3: Every Component a Complete Framework Must Answer For

Builds on [01-crash-course.md](01-crash-course.md) and [02-strengths-and-shortcomings.md](02-strengths-and-shortcomings.md).

**What "have an answer for" means:** for every item below, a user of a truly complete framework should never have to research which library to use or how to wire it. The answer can be one of three things, and choosing which is part of the debate we have next:

- **Core:** built into the framework itself.
- **Battery:** a first-party module that shares the same conventions, is optional, and is maintained with the core.
- **Seam:** a defined, stable integration point plus an official recommendation (e.g. "the JS bundler is yours; here is the contract").

Each component has an ID so we can refer to it when arguing. Under each one, **Must answer** lists the questions it has to settle, and **Multi-node** says what changes once the app runs as many processes on many nodes. That's your cross-cutting replication requirement, applied everywhere instead of in one chapter.

There are 24 domains and 216 components. If something is missing, it is a bug in this list. Tell me and I'll add it.

---

## A. Kernel, lifecycle and process model

**A1. Application kernel.** The root object that owns everything.
- Must answer: How is an app constructed? How are modules registered? What is the boot order? Can the same app run as an HTTP server, a worker, a CLI command, or a test harness?
- Multi-node: The kernel knows its node identity, its role(s), and its cluster membership.

**A2. Lifecycle hooks.** Init, start, ready, drain, stop.
- Must answer: Ordered startup with dependencies? Startup timeouts? Fail-fast? Ordered shutdown in reverse?
- Multi-node: "Ready" must only flip after the node has joined the cluster and caught up. "Drain" must hand off work (leases, sockets) before stopping.

**A3. Graceful shutdown and draining.**
- Must answer: In-flight requests, long-lived connections (WebSocket, SSE, gRPC streams), running jobs, and buffered telemetry all finish or are handed off within a deadline.
- Multi-node: Tell load balancers first (readiness off), then tell clients to reconnect elsewhere.

**A4. Process model.**
- Must answer: Single process, multi-process on one host (prefork / `SO_REUSEPORT`, a supervisor with worker processes), and role separation (web, worker, scheduler, realtime) from one binary.
- Multi-node: The same model scales from 1 process to N nodes without code changes. The only difference is the coordination backend.

**A5. Zero-downtime restart and reload.**
- Must answer: Socket handoff or `SO_REUSEPORT` overlap, config reload without restart, rolling restarts.
- Multi-node: Coordinated rolling restarts so the cluster never loses quorum or capacity.

**A6. Dependency wiring (DI).**
- Must answer: How do components get their dependencies? Compile-time (codegen or generics) or runtime? Scopes (singleton, per-request, per-job)? How are wiring errors reported, and when?
- Multi-node: Per-node singletons vs cluster-wide singletons (see O6, leader election).

**A7. Modules and plugins.**
- Must answer: What is a module (routes, services, migrations, jobs, config, templates, assets)? How is it encapsulated? Can third parties ship modules? How do modules depend on each other?

**A8. Resource management.**
- Must answer: Goroutine limits, memory limits (`GOMEMLIMIT`), `GOMAXPROCS` in containers, file-descriptor limits, connection pool sizing.

**A9. Panic and crash policy.**
- Must answer: Recover per request, per job, per goroutine the framework spawns. When to crash the process instead. Crash reports.

---

## B. Configuration, secrets and flags

**B1. Configuration loading.**
- Must answer: Sources (files, env, flags, remote), formats, precedence, typed structs, defaults, validation at boot, per-environment profiles.

**B2. Config introspection.**
- Must answer: "What config is this process actually running with, and where did each value come from?" Secrets must be redacted.

**B3. Dynamic configuration.**
- Must answer: Which values can change at runtime, how changes are watched, and how components react.
- Multi-node: Propagation to all nodes, with consistency guarantees ("all nodes see the change within X").

**B4. Secrets management.**
- Must answer: Loading from env, files, or vaults (Vault, AWS/GCP secret managers), rotation without restart, never logging secrets.
- Multi-node: Every node rotates in step.

**B5. Feature flags.**
- Must answer: Boolean and percentage rollouts, targeting (user, tenant), kill switches, an OpenFeature-compatible seam.
- Multi-node: Consistent evaluation across nodes.

**B6. Environment awareness.**
- Must answer: dev, test, staging, prod, with sane, safe defaults per environment (e.g. detailed errors only in dev).

---

## C. Transport and protocols

**C1. HTTP/1.1 and HTTP/2** (h2 over TLS, and h2c for internal use).

**C2. HTTP/3 / QUIC.**

**C3. TLS.**
- Must answer: Certificate loading and hot reload, ACME (Let's Encrypt), mTLS for services, modern defaults.
- Multi-node: The ACME challenge and the certificate store must be shared across nodes.

**C4. WebSockets.**
- Must answer: Upgrade, ping/pong, compression, limits, backpressure, auth on connect.

**C5. Server-Sent Events.**

**C6. gRPC.**
- Must answer: Serving to services **and to external clients (mobile)**. Streaming in all four modes, deadlines, metadata, reflection, health service.

**C7. gRPC-Web and Connect protocol.** Browser and curl-friendly RPC from the same handler.

**C8. GraphQL.**
- Must answer: Schema-first or code-first, subscriptions, dataloaders, complexity limits. Probably a Battery or Seam.

**C9. JSON-RPC and other RPC styles.** Including serving MCP (Model Context Protocol) endpoints, which apps are increasingly expected to expose.

**C10. Raw TCP/UDP and custom protocols.**
- Must answer: Can a Galleon service host a non-HTTP listener with the same lifecycle, middleware-ish hooks, and observability?

**C11. WebTransport.** The future-facing one: low-latency bidirectional streams over HTTP/3.

**C12. Listener features.**
- Must answer: Unix sockets, PROXY protocol, multiple listeners and ports, port sharing across processes, socket activation (systemd).

**C13. Compression.** gzip, brotli, zstd, negotiated, with thresholds and pre-compressed static assets.

**C14. Outbound HTTP client.**
- Must answer: A first-class client with the same middleware model as the server: timeouts, retries, circuit breaking, tracing propagation, auth injection, mocking in tests.

---

## D. Routing

**D1. Path matching.** Static, params, wildcards, regex or type constraints, priority rules, conflict detection at boot.

**D2. Method handling.** Automatic `HEAD`/`OPTIONS`, `405` with `Allow`, method override for HTML forms.

**D3. Groups, mounting and sub-routers.** Prefixes and middleware scoped to a group.

**D4. Host and subdomain routing.** Including wildcard tenants (`{tenant}.example.com`).

**D5. Reverse routing.** Generate URLs from route names with type-safe params, including absolute URLs and signed URLs.

**D6. Route introspection.** List all routes with their middleware, handlers, and contracts, both from the CLI and at runtime.

**D7. Versioning.** Path, header, or media-type versioning, plus deprecation headers (`Sunset`, `Deprecation`).

**D8. Static files.** Embedded (`embed.FS`) or on disk, with cache headers, ETags, range requests, and fingerprinted asset URLs.

**D9. Trailing slashes, case sensitivity, and redirects.** Explicit, configurable policy.

**D10. Resource routing conventions.** A Rails-style `resources` helper for REST.

**D11. File-system routing (optional).** Next.js-style routing for page-oriented full-stack apps.

---

## E. Request pipeline and middleware

**E1. Middleware model.**
- Must answer: What's the signature? Is it compatible with `func(http.Handler) http.Handler`? Ordering, per-route vs global, and one model for HTTP, RPC, jobs, and the client?

**E2. Lifecycle vocabulary.** Named stages, e.g. guard, before, bind, validate, handle, after, error, finish. Every team speaks the same language.

**E3. Context propagation.**
- Must answer: Request-scoped values (typed, not `any`), cancellation, deadlines, and propagation into jobs, events, and outbound calls.

**E4. Request ID and correlation.** Generated or accepted, propagated everywhere, and returned in responses.

**E5. Timeouts and deadlines.** Server-wide, per route, propagated from clients (`grpc-timeout`), and enforced on downstream calls.

**E6. Body limits and slow-client protection.** Max sizes, read/write timeouts, and slowloris defense.

**E7. CORS.**

**E8. Conditional requests.** ETag, `Last-Modified`, `If-None-Match`, `If-Match` for optimistic concurrency.

**E9. Rate limiting.**
- Must answer: Per IP, user, key, or route. Algorithms (token bucket, sliding window). Standard headers.
- Multi-node: Global limits across all nodes, not per process. This is the classic bug.

**E10. Idempotency keys.** `Idempotency-Key` handling so retried POSTs are safe.
- Multi-node: The key store has to be shared across nodes.

**E11. Request coalescing and singleflight.** Deduplicate concurrent identical work.
- Multi-node: Optionally, cluster-wide deduplication.

**E12. Load shedding and concurrency limits.** Adaptive limits, priority-based shedding, `503` with `Retry-After`.

**E13. Trusted proxies and client IP.** Correct `X-Forwarded-*` / `Forwarded` handling.

---

## F. Input: binding and validation

**F1. Typed extraction.** Path, query, headers, cookies, body, form, and multipart into typed structs or extractors, with no god context.

**F2. Content-type decoding.** JSON, form, multipart, XML, protobuf, msgpack, CBOR. Pluggable.

**F3. Streaming input.** Large bodies and uploads streamed, not buffered. Resumable uploads (tus).

**F4. File uploads.** Size and type limits, virus-scan hook, direct-to-storage (presigned) uploads.

**F5. Validation.**
- Must answer: Declarative rules, cross-field rules, custom rules, async rules (e.g. "email is unique"), where validation lives (struct, schema, or changeset-style), and error format.

**F6. Validation messages.** Field-level errors, localized and machine-readable.

**F7. Sanitization and normalization.** Trim, case, Unicode normalization, HTML sanitization.

**F8. Changesets / input-to-model mapping.** The Ecto-style separation of "what the user sent" from "what we store", including partial updates (PATCH semantics).

---

## G. Output: rendering and serialization

**G1. Serialization.** JSON (fast, ideally precompiled or codegen), protobuf, msgpack, XML, CSV, NDJSON.

**G2. Content negotiation.** `Accept`-driven choice of format and language.

**G3. Streaming output.** Chunked responses, streaming JSON, file downloads, range responses, and backpressure.

**G4. Response shaping.** Field selection, envelopes vs bare responses, consistent conventions.

**G5. Pagination.** Cursor and offset conventions, `Link` headers, typed helpers.

**G6. HTTP caching headers.** `Cache-Control`, `Vary`, surrogate keys for CDN purging.

**G7. HTML rendering.** See M.

---

## H. Errors

**H1. Error model.** One error type carrying code, message, HTTP status, gRPC status, details, and cause.

**H2. Error mapping.** Domain errors map to transport errors (HTTP status, gRPC code, problem+json/RFC 9457) in one place.

**H3. Error catalog.** Stable, documented error codes that clients (including mobile) can switch on.

**H4. Safe exposure.** Internal details hidden in prod, shown in dev with a rich debug page.

**H5. Error reporting.** Sentry-style capture with context, deduplication, and rate limiting.

---

## I. Contracts, API description and clients

**I1. Single source of truth.**
- Must answer: Is the contract Go types, protobuf, OpenAPI, or a DSL? Everything else derives from it.

**I2. OpenAPI 3.1 generation.** Including examples, auth schemes, and error schemas.

**I3. Protobuf / gRPC schema.** Generated from Go, or Go generated from proto, or both directions.

**I4. AsyncAPI / event schemas.** Contracts for events and messages, not just requests.

**I5. Client generation.** Go, TypeScript, Swift, Kotlin, and Dart (mobile), with streaming support. Ideally tRPC-feel for TS.

**I6. API docs UI.** Interactive docs (Scalar, Swagger UI, or similar) served by the app.

**I7. Breaking-change detection.** Diff contracts in CI and fail on incompatible changes.

**I8. API versioning and deprecation lifecycle.**

**I9. Mock server and contract testing.** Clients can develop against a mock generated from the contract.

---

## J. Data

**J1. Database connectivity.** Drivers, pooling, health checks, timeouts, statement caching, Postgres/MySQL/SQLite at minimum.

**J2. Query layer.**
- Must answer: SQL-first (sqlc-style), query builder, ORM, or more than one? Type safety, N+1 prevention.

**J3. Migrations.**
- Must answer: Authoring (SQL or Go), up/down, generated from model diffs or not, zero-downtime patterns (expand/contract), data migrations.
- Multi-node: Run exactly once across the cluster, with a lock, before new code serves traffic.

**J4. Transactions and unit of work.** Context-scoped transactions, nested savepoints, retries on serialization failures.

**J5. Read replicas, routing and sharding.** Read/write splitting, replica lag awareness ("read your writes").

**J6. Multi-tenancy at the data layer.** Shared schema with tenant column, schema per tenant, database per tenant. Automatic scoping.

**J7. Seeding, fixtures and factories.**

**J8. Auditing, soft deletes and history.** Created/updated by, temporal tables or change logs.

**J9. Transactional outbox and inbox.** Reliable event publishing tied to DB commits, and deduplicated consumption.

**J10. Caching.**
- Must answer: In-process, distributed, multi-level (L1 + L2), cache-aside and read-through, stampede protection, tagging and invalidation.
- Multi-node: Invalidation must reach every node's L1 (via pub/sub).

**J11. Key-value and document stores.** Redis, DynamoDB, Mongo, etc. Seam with official adapters.

**J12. Object and file storage.** A filesystem abstraction over local, S3, GCS, and Azure, with presigned URLs, streaming, and attachments linked to records.
- Multi-node: Local disk is never the default once there is more than one node.

**J13. Search.** Full-text (Postgres FTS, Meilisearch, Elasticsearch, Typesense), with indexing kept in sync with data.

**J14. Data privacy.** PII tagging, encryption at field level, GDPR export and erase, retention policies.

**J15. Data import and export.** CSV and Excel import/export and bulk operations.

---

## K. Identity, security and access

**K1. Sessions.**
- Must answer: Cookie or server-side store, rotation, fixation protection, expiry, "log out everywhere".
- Multi-node: The store is shared across nodes, with no sticky sessions required.

**K2. Authentication methods.** Passwords (argon2id/bcrypt), magic links, OTP, passkeys/WebAuthn, MFA (TOTP), social login (OAuth2/OIDC client), SSO (SAML/OIDC for enterprise).

**K3. Tokens.** JWT and PASETO issuing and verification, opaque tokens, refresh flows, revocation.
- Multi-node: Revocation lists and signing keys are shared across nodes.

**K4. API keys and service credentials.** Scoped and hashed keys, rotation, last-used tracking.

**K5. Acting as an identity provider.** Being an OAuth2/OIDC server for your own mobile apps and third parties. Probably a Battery.

**K6. Service-to-service identity.** mTLS, SPIFFE, signed service tokens.

**K7. Authorization.** RBAC, ABAC, ReBAC (Zanzibar-style), policy definitions, enforcement at route, handler, and data level, and "can this user do X?" in templates.

**K8. User and account management.** Registration, verification, recovery, profile, organizations and teams, invitations, impersonation (admin "log in as").

**K9. CSRF protection.** Token or origin-based, on by default for cookie auth.

**K10. Security headers.** CSP (with nonces), HSTS, frame options, referrer policy, permissions policy. On by default.

**K11. Output escaping and XSS defense.** Context-aware auto-escaping in templates.

**K12. SSRF and egress protection.** For outbound client calls to user-supplied URLs.

**K13. Cryptography and keys.** Encryption helpers, key rings, rotation, signed values (cookies, URLs).
- Multi-node: The key ring is shared across nodes, like ASP.NET Data Protection.

**K14. Brute-force and abuse protection.** Login throttling, lockout, captcha and bot-protection seams.

**K15. Audit logging.** Who did what, when, tamper-evident, queryable.

**K16. Supply-chain and dependency hygiene.** Minimal dependencies, `govulncheck` integration, SBOM.

---

## L. Background work and messaging

**L1. Job queue.**
- Must answer: Enqueue in the same transaction as data, retries with backoff, priorities, delays, uniqueness, timeouts, dead-letter queues, batch and chain jobs, rate-limited queues.
- Multi-node: Exactly-once or at-least-once with idempotency, safe concurrent workers across nodes, lease recovery when a node dies.

**L2. Scheduler / cron.**
- Must answer: Cron expressions, intervals, time zones, missed-run policy, overlap prevention.
- Multi-node: Runs once per tick across the cluster (Laravel's `onOneServer()`), not once per node.

**L3. In-process events.** Domain events, sync and async listeners, ordering.

**L4. Message broker integration.** Kafka, NATS, RabbitMQ, SQS/SNS, Google Pub/Sub. Consumers with the same handler model and middleware as HTTP.
- Multi-node: Consumer groups and partition assignment.

**L5. Durable workflows and sagas.** Long-running, resumable, multi-step processes (Temporal-style), compensation.

**L6. Job dashboard.** Inspect, retry, cancel, and pause jobs and queues (Horizon / Sidekiq UI).

**L7. Batching and debouncing.** Aggregate work over time windows.

---

## M. Realtime

**M1. Channels and topics.** Subscribe and publish over WebSocket or SSE, with auth per channel.

**M2. Cluster-wide pub/sub.**
- Multi-node: A message published on node A reaches subscribers on node B. This is the Phoenix PubSub bar.

**M3. Presence.** Who's online, in which room, consistent across nodes (CRDT-style).

**M4. Delivery semantics.** Ordering, acknowledgements, resume after reconnect (last-event-id, offsets), backpressure, slow-consumer policy.

**M5. Server-driven UI.** A LiveView-style model where the server holds state and pushes diffs.
- Multi-node: Reconnects may land on a different node, so state must be recoverable.

**M6. Client SDKs for realtime.** JS, and mobile (Swift, Kotlin, Dart) for your mobile users.

---

## N. Frontend and full-stack

**N1. Templating.** Type-safe, compiled components (templ-like) or `html/template`, with layouts, partials, slots, and auto-escaping.

**N2. Component model.** Reusable UI components with props, composition, and colocated assets.

**N3. Forms.** Rendering from schemas, validation error round-trip, old-input preservation, CSRF tokens, file inputs, multi-step forms.

**N4. Flash messages and redirects.** Post/redirect/get helpers.

**N5. Partial updates.** htmx/Hotwire-style fragment responses, out-of-band swaps.

**N6. Islands and interactivity.** Mounting client-side components inside server-rendered pages.

**N7. Asset pipeline.** CSS/JS bundling (esbuild, Vite, Tailwind), fingerprinting, embedding into the binary, source maps, CDN upload.

**N8. Dev experience.** Hot reload of Go code, live reload of templates and CSS, HMR via the bundler seam.

**N9. SPA and meta-framework integration.** Serving a Vite/React/Svelte SPA, SSR proxying to Next.js/SvelteKit/Nuxt, typed clients from contracts, dev proxying.

**N10. Internationalization and localization.** Message catalogs, pluralization (ICU), locale detection, localized routes, formatting dates, numbers, and currency, right-to-left support.

**N11. SEO and metadata.** Titles and meta tags, Open Graph, sitemaps, robots.txt, canonical URLs, structured data.

**N12. Images and media.** Resizing, format conversion (WebP/AVIF), responsive `srcset`, on-the-fly vs pre-generated.

**N13. Accessibility affordances.** Accessible defaults in generated components and scaffolds.

**N14. Admin panel.** Django-admin style: CRUD screens generated from models and contracts, customizable, with authorization.

---

## O. Distribution, inter-service and resilience

**O1. Cluster membership.** Nodes discover each other (static, DNS, Kubernetes API, gossip, Consul/etcd). Failure detection.

**O2. Coordination backend abstraction.** One interface for pub/sub, locks, leader election, shared KV, and counters, with in-process, Postgres, Redis, NATS, and etcd backends. Every multi-node note in this list ultimately rests on this.

**O3. Distributed locks and leases.** With fencing tokens and TTLs.

**O4. Leader election and singleton tasks.**

**O5. Work partitioning.** Consistent hashing or sharded ownership (e.g. "this node owns tenants 1-100"), rebalancing on membership change.

**O6. Cluster-wide singletons vs node-local singletons.** An explicit API for both.

**O7. Service discovery for clients.** DNS, Kubernetes, Consul, and static. Client-side load balancing.

**O8. Resilience policies.** Timeouts, retries with jitter and budgets, circuit breakers, bulkheads, hedged requests, fallbacks, defined once and applied to any client.

**O9. Backplanes.** Explicit per-component backplanes (sessions, cache, realtime, rate limit) that default to the O2 backend.

**O10. Multi-region awareness.** Region and zone labels, locality-aware routing, data residency hooks.

**O11. Service mesh compatibility.** Works cleanly behind Envoy/Istio/Linkerd without fighting them.

**O12. Inter-process communication on one host.** Shared-port prefork, plus an IPC channel between local workers (cheaper than the network path).

---

## P. Observability and operations

**P1. Structured logging.** `slog`-based, with request and trace correlation, redaction, sampling, and level control at runtime.

**P2. Metrics.** RED/USE metrics for every component by default, with OpenTelemetry and Prometheus exposition.

**P3. Distributed tracing.** OpenTelemetry spans across HTTP, RPC, DB, cache, jobs, and messaging, with propagation through queues.

**P4. Profiling.** pprof endpoints (protected), continuous profiling seam (Pyroscope/Parca), PGO profile capture.

**P5. Health checks.** Liveness, readiness, startup, and dependency checks, with Kubernetes-ready semantics.

**P6. Operational endpoints (Actuator).** Info, build version, config, routes, jobs, cluster members, and feature flags, protected and toggleable.

**P7. Dev dashboard (Telescope-style).** In dev, inspect every request, query, job, mail, cache hit, and log line.

**P8. Error tracking.** See H5.

**P9. SLOs and alerting hooks.** Defining service-level objectives in code, with default dashboards exportable for Grafana.

**P10. Runtime diagnostics.** Goroutine dumps, pool stats, GC stats, and slow-query and slow-request logs.

---

## Q. Communication outbound

**Q1. Mail.** Templates (HTML + text), multiple providers (SMTP, SES, Postmark, etc.), queue-backed sending, previews in dev, testing fakes.

**Q2. SMS and voice.** Provider seam (Twilio etc.).

**Q3. Push notifications.** APNs and FCM for your mobile users, plus Web Push. Device token management.

**Q4. Notification center.** One "notify this user" API that fans out to mail, push, SMS, in-app, and Slack, with user preferences and digests.

**Q5. Outbound webhooks.** Signed payloads, retries with backoff, delivery logs, replay, endpoint management for customers.

**Q6. Inbound webhooks.** Signature verification for common providers (Stripe, GitHub, etc.), idempotent processing.

---

## R. Performance and efficiency

**R1. Hot-path allocation discipline.** Pooling, zero-copy where safe, no reflection per request.

**R2. Precompiled work.** Routes, serializers, validators, and DI graphs built at compile or boot time, never per request.

**R3. Connection management.** Keep-alive tuning, HTTP/2 multiplexing, DB pool sizing.

**R4. Response caching.** Full-page and fragment caching, `stale-while-revalidate`, CDN integration.

**R5. Benchmark suite.** Published, reproducible benchmarks covering realistic workloads, not just "hello world".

**R6. Profile-guided optimization.** Easy PGO build pipeline from production profiles.

**R7. Startup time and memory footprint.** Fast boot for serverless and autoscaling.

---

## S. Multi-tenancy and product scaffolding

**S1. Tenancy model.** Tenant resolution (subdomain, header, path, token) feeding data scoping (J6), config, feature flags, rate limits, and storage.

**S2. Organizations, teams and memberships.** Common SaaS primitives (see K8).

**S3. Billing seam.** Stripe/Paddle integration patterns: subscriptions, webhooks, entitlements. Probably Seam only.

**S4. Settings and preferences.** Per-user and per-tenant settings storage with typed access.

**S5. Usage metering.** Counting usage for quotas and billing.
- Multi-node: Counters aggregated across nodes.

---

## T. Testing

**T1. Handler testing.** Fluent request builders and response assertions, in-memory, with no network.

**T2. Fakes for every component.** Mail, queue, storage, clock, cache, push, HTTP client, and cluster, all swappable with one line.

**T3. Database testing.** Per-test transactions or templates, fast parallel isolated databases, factories.

**T4. Time control.** An injectable clock everywhere, so tests can travel through time.

**T5. Contract tests.** Verify the server against its contract, and clients against mocks.

**T6. End-to-end and browser tests.** Playwright seam, app booted in test mode.

**T7. Cluster tests.** Spin up N nodes in one test process to verify multi-node behaviour (locks, pub/sub, cron singletons, failover). Rare in other frameworks, but required by your replication goal.

**T8. Load and chaos testing.** Hooks for k6 or similar. Fault injection (latency, errors, node loss).

**T9. Snapshot and golden testing.** For HTML, JSON, and contracts.

---

## U. Developer experience and tooling

**U1. CLI.** `galleon new`, `dev`, `build`, `generate`, `migrate`, `routes`, `console`, `doctor`, and so on.

**U2. Project templates.** Starters for an API service, an internal gRPC service, a full-stack app, a worker, and a monorepo of services.

**U3. Generators and scaffolding.** Resources, handlers, models, migrations, jobs, mailers, and components, producing plain readable Go.

**U4. Dev server.** Rebuild on change, live reload, error overlay in the browser, and local dependencies (DB, Redis) via containers.

**U5. Interactive console / REPL.** Run code against the app's services (Rails console equivalent). Hard in Go, which is part of the debate.

**U6. Static analyzers and linters.** Galleon-specific `go vet` analyzers that catch misuse at compile time (unregistered routes, missing auth, unsafe patterns).

**U7. Error message quality.** Boot-time errors that say exactly what is wrong and how to fix it.

**U8. Upgrade tooling.** Codemods for breaking changes, deprecation warnings well ahead of removal.

**U9. Documentation.** Guides, reference, cookbook, and runnable examples, versioned alongside the code.

**U10. IDE and LLM friendliness.** Navigable code (no magic), `llms.txt`, an MCP server that lets AI tools inspect routes, contracts, and config.

**U11. App self-description.** The app can emit a full manifest of itself: routes, contracts, jobs, crons, events, dependencies, config. The Encore idea, but from plain Go.

---

## V. Build, packaging and deployment

**V1. Single static binary.** Assets, templates, and migrations embedded, with one binary for all roles.

**V2. Container images.** Minimal (distroless/scratch), multi-arch, reproducible builds.

**V3. Deploy targets.** VMs (systemd), Kubernetes (manifests or Helm generated from the app manifest), PaaS (Fly, Render, Railway), and serverless (Lambda, Cloud Run) where it makes sense.

**V4. Release workflow.** Migrations before traffic, health-gated rollouts, blue/green and canary support.

**V5. Infrastructure from code.** Optionally generating infra definitions (Terraform/Pulumi, k8s) from the app manifest (U11). An Encore-style capability without the lock-in.

**V6. Cross-compilation and platforms.** Linux, macOS, Windows, ARM.

---

## W. AI and emerging capabilities

**W1. LLM streaming responses.** First-class token streaming over SSE, WebSocket, and gRPC streams, with cancellation.

**W2. Serving MCP tools.** Exposing app operations as MCP tools with auth.

**W3. AI provider seam.** Probably only a recommendation, not a component.

---

## X. Governance and meta-components

These aren't code, but a framework without answers here doesn't survive (the Gorilla/Buffalo lesson):

**X1. Compatibility promise.** SemVer, a Go 1-style stability guarantee for the core, and LTS releases.

**X2. Modularity contract.** Every component usable standalone (the Symfony lesson). Clear dependency direction.

**X3. Extension API.** How third parties build modules, middleware, and backends, plus a registry.

**X4. Dependency policy.** Minimal third-party dependencies in the core. Critical ones are vendored, forked, or owned.

**X5. Security process.** Disclosure policy, advisories, audits.

**X6. Maintainer and sustainability model.** Bus factor, funding, governance.

**X7. License.**

---

## Summary by domain

| Domain | Components |
|---|---|
| A. Kernel, lifecycle, process | 9 |
| B. Configuration, secrets, flags | 6 |
| C. Transport and protocols | 14 |
| D. Routing | 11 |
| E. Request pipeline | 13 |
| F. Input | 8 |
| G. Output | 7 |
| H. Errors | 5 |
| I. Contracts and clients | 9 |
| J. Data | 15 |
| K. Identity and security | 16 |
| L. Background work and messaging | 7 |
| M. Realtime | 6 |
| N. Frontend and full-stack | 14 |
| O. Distribution and resilience | 12 |
| P. Observability and operations | 10 |
| Q. Outbound communication | 6 |
| R. Performance | 7 |
| S. Multi-tenancy and product | 5 |
| T. Testing | 9 |
| U. Developer experience | 11 |
| V. Build and deploy | 6 |
| W. AI and emerging | 3 |
| X. Governance | 7 |
| **Total** | **216** |

### Components that change once the app replicates

These are the ones whose design is shaped by your multi-process, multi-node requirement, and where most frameworks quietly break:

A2 ready/drain, A4 process model, A5 rolling restarts, B3 dynamic config, B4 secret rotation, B5 flags, C3 ACME and certs, E9 rate limits, E10 idempotency, E11 coalescing, J3 migrations, J10 cache invalidation, J12 file storage, K1 sessions, K3 token revocation, K13 key ring, L1 job leases, L2 cron, L4 consumer groups, M2 pub/sub, M3 presence, M5 server-driven UI state, O1 to O12 entirely, S5 usage counters, T7 cluster tests.

That's about 40 components, all of which point at the same root dependency: **O2, the coordination backend**.

---

## Next

Combine the lessons from parts 1 and 2 with this list to discuss your desires and fight over their merit. A good way to start: tell me what you want Galleon to be, as loosely or as specifically as you like. I'll push back with the trade-offs and the Core/Battery/Seam call for each area.
