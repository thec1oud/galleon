# Galleon, Part 2: Strengths and Shortcomings of the Prior Art

Builds on [01-crash-course.md](01-crash-course.md). This scores each piece of prior art against the 17 layers, then goes through what each one does well and badly, and ends with the patterns that repeat across all of them.

All of this comes from general knowledge of these projects, not fresh benchmarking or a survey. Treat the scorecard as a fair approximation, not a measurement.

---

## 1. Scorecard against the 17 layers

Legend: **●** built in and first class · **◐** partial, or an official add-on · **○** bring your own

| | Transport | Routing | Middleware | Binding/validation | Rendering | Errors | Contracts | DI/lifecycle | Data | Auth | Jobs | Frontend | Observability | Testing | Tooling | Inter-service | Replication |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| net/http | ◐ | ◐ | ◐ | ○ | ◐ | ○ | ○ | ○ | ◐ | ○ | ○ | ◐ | ◐ | ● | ○ | ○ | ○ |
| Gin | ◐ | ● | ● | ● | ● | ◐ | ○ | ○ | ○ | ◐ | ○ | ◐ | ◐ | ◐ | ○ | ○ | ○ |
| Echo | ◐ | ● | ● | ● | ● | ● | ○ | ○ | ○ | ◐ | ○ | ◐ | ◐ | ◐ | ○ | ○ | ○ |
| Fiber | ◐ | ● | ● | ● | ● | ● | ○ | ○ | ○ | ◐ | ○ | ◐ | ◐ | ◐ | ○ | ○ | ◐ |
| Chi | ◐ | ● | ● | ○ | ○ | ○ | ○ | ○ | ○ | ◐ | ○ | ○ | ◐ | ● | ○ | ○ | ○ |
| Beego | ◐ | ● | ● | ● | ● | ◐ | ◐ | ◐ | ● | ◐ | ◐ | ● | ◐ | ◐ | ● | ○ | ○ |
| Connect / gRPC | ● | ◐ | ● | ● | ◐ | ● | ● | ○ | ○ | ◐ | ○ | ○ | ◐ | ◐ | ● | ● | ◐ |
| go-zero / Kratos | ● | ● | ● | ● | ◐ | ● | ● | ◐ | ◐ | ◐ | ◐ | ○ | ● | ◐ | ● | ● | ◐ |
| Encore | ◐ | ● | ● | ● | ◐ | ● | ● | ● | ● | ◐ | ● | ○ | ● | ● | ● | ● | ● |
| Rails | ◐ | ● | ● | ● | ● | ● | ○ | ◐ | ● | ● | ● | ● | ◐ | ● | ● | ○ | ◐ |
| Django | ◐ | ● | ● | ● | ● | ● | ○ | ◐ | ● | ● | ◐ | ● | ◐ | ● | ● | ○ | ◐ |
| Laravel | ◐ | ● | ● | ● | ● | ● | ○ | ● | ● | ● | ● | ● | ● | ● | ● | ○ | ◐ |
| Spring Boot | ● | ● | ● | ● | ● | ● | ◐ | ● | ● | ● | ◐ | ◐ | ● | ● | ● | ● | ◐ |
| ASP.NET Core | ● | ● | ● | ● | ● | ● | ● | ● | ● | ● | ◐ | ● | ● | ● | ● | ● | ◐ |
| Phoenix | ◐ | ● | ● | ● | ● | ● | ○ | ◐ | ● | ◐ | ◐ | ● | ● | ● | ● | ◐ | ● |
| NestJS | ◐ | ● | ● | ● | ◐ | ● | ● | ● | ◐ | ◐ | ◐ | ○ | ◐ | ● | ● | ● | ◐ |
| Next.js | ◐ | ● | ◐ | ◐ | ● | ◐ | ○ | ○ | ○ | ◐ | ○ | ● | ◐ | ◐ | ● | ○ | ◐ |
| Axum | ● | ● | ● | ● | ◐ | ● | ○ | ○ | ○ | ○ | ○ | ○ | ◐ | ◐ | ○ | ◐ | ○ |

**What jumps out:**

- **No Go project fills the row.** Encore comes closest, and it does that with its own toolchain and no frontend story.
- **ASP.NET Core and Laravel are the most complete overall.** They are the bar for "complete through and through".
- **Replication is weak almost everywhere.** Only Phoenix (thanks to the BEAM) and Encore (thanks to provisioning infra) treat it as built in. Everyone else is stateless-and-hope or plug in Redis. This is a real gap Galleon can own.
- **Contracts are absent from every full-stack framework.** The API frameworks have them and the full-stack frameworks don't. Nobody does both well.
- **Client-facing gRPC** (your mobile case) is only first class in Connect, ASP.NET Core and Spring. Connect is the cleanest, because one handler serves gRPC, gRPC-Web and JSON.

---

## 2. Go prior art, one by one

### net/http
**Does well**
- It is the universal interface. Every Go library speaks `http.Handler`.
- It is stable and secure, follows the Go 1 compatibility promise, and has HTTP/2 built in.
- Since Go 1.22 it has a good enough router, `httptest` is excellent, and `slog` covers logging.

**Falls short**
- Handlers can't return errors, so every handler repeats the same error-writing code.
- It has no binding, validation, error model, contracts, DI, or lifecycle beyond `Shutdown`.
- It has no HTTP/3 (that needs `quic-go`), and it has no reverse routing (URL generation).
- **Lesson:** keep it as the foundation and never replace it. Build the ergonomics on top.

### Gin
**Does well**
- It has the largest ecosystem and the most hiring familiarity. It is fast enough and easy to learn.
- Binding with tags and validator is convenient.

**Falls short**
- Handlers return nothing, so error handling relies on `c.Error()` and `c.Abort()` conventions that are easy to get wrong.
- `*gin.Context` is a god object. It doesn't satisfy `context.Context` cleanly, and its key/value store uses `any`, so it isn't type-safe.
- Middleware isn't `http.Handler`, which splits the ecosystem.
- It has no answer for anything beyond HTTP plumbing, its OpenAPI support (swaggo) comes from comments, and its pace of change is slow.
- **Lesson:** popularity comes from being easy on day 1. The god-context pattern hurts on day 1000.

### Echo
**Does well**
- Returning `error` from handlers, combined with a central error handler, is the right design.
- It has a broad, consistent set of first-party middleware, a mockable context interface, and good docs.

**Falls short**
- It has the same custom-context lock-in as Gin and the same lack of anything beyond HTTP.
- Binding is less flexible than extractors.
- Its slower release cadence has worried users at times.
- **Lesson:** steal the error model.

### Fiber
**Does well**
- Its raw throughput and low allocation are excellent, and the Express-like API is familiar to Node developers.
- **Prefork** (many processes sharing one port via `SO_REUSEPORT`) is one of the few Go-side answers to multi-process.
- It has a big first-party middleware set.

**Falls short**
- It is built on fasthttp, so it's cut off from the `net/http` ecosystem. It has no HTTP/2 or gRPC, and OpenTelemetry and many libraries need adapters.
- Buffer reuse makes it unsafe to keep values from the context, which causes subtle bugs.
- Benchmark wins rarely matter once a database is involved.
- **Lesson:** efficiency must not cost compatibility or safety. Prefork is a good idea that belongs in a `net/http` world.

### Chi
**Does well**
- It is idiomatic and fully `http.Handler` compatible.
- Its sub-router composition (`Mount`, `Route`, `Group`, `With`) is the best in Go for large route trees.
- It is tiny, stable, and well tested.

**Falls short**
- It deliberately does nothing else. Everything from binding to jobs is assembly work, and every team assembles it differently.
- Go 1.22's `ServeMux` has eroded its reason to exist.
- **Lesson:** this is the composition model to adopt. It just needs a framework on top.

### Gorilla
**Does well**
- Its focused, reusable packages are still everywhere (websocket, sessions, securecookie, csrf).

**Falls short**
- The whole ecosystem depended on a few volunteers, and the project was archived for a while.
- **Lesson:** a "complete" framework needs a sustainability plan, and it should own its critical dependencies.

### Beego
**Does well**
- It is the most complete Go full-stack framework, with an ORM, CLI, hot reload, admin monitoring, config, cache, and sessions.

**Falls short**
- Controller-struct MVC and annotation-by-comment routing feel foreign in Go.
- Its ORM is weaker than modern choices like sqlc or ent, it uses globals, and its parts are tightly coupled.
- Its English docs and community are thinner, and it has no contracts or modern frontend story.
- **Lesson:** "complete" alone doesn't win. Completeness has to come with Go idioms and modern quality.

### Buffalo and Revel
**Did well**
- Buffalo had generators, hot reload, migrations, the asset pipeline, and resources: genuine Rails-level productivity.
- Revel had hot reload and a full lifecycle early on.

**Fell short**
- They coupled to fast-moving frontend tooling (webpack), which became a maintenance sink.
- They bundled their own ORM and templating instead of the best-in-class choices.
- They depended on one or two maintainers, and Revel had a lot of reflection magic.
- Both stalled.
- **Lesson:** don't own the JS toolchain. Integrate with it at a stable seam. Keep the core small enough for a team to maintain.

### Goravel
**Does well**
- It aims at Laravel's breadth (queues, events, mail, scheduler, auth) and proves the demand exists.

**Falls short**
- Facades and a service container translated literally from PHP feel unidiomatic, and a lot of the wiring happens at runtime.
- It is young and has a small community.
- **Lesson:** copy Laravel's *scope*, not its *mechanisms*.

### gRPC-Go
**Does well**
- It has strict contracts, efficient binary encoding, and streaming, with deadlines and cancellation built in.
- It is mature, comes with load balancing and health checks, and is the industry standard for internal RPC.
- It is great for mobile clients on paper, since compact payloads and streaming suit them.

**Falls short**
- It runs its own server, separate from `net/http`, and needs HTTP/2 end to end.
- Browsers need a proxy, and debugging with curl is painful.
- Its generated code is verbose, and its API surface is large.
- **Lesson:** the protocol is right. Its packaging is too isolated.

### Connect
**Does well**
- One `http.Handler` speaks gRPC, gRPC-Web, and Connect (JSON over HTTP/1.1 or 2), so it suits mobile, browsers, and services at once.
- It mounts beside normal routes, uses generic types, and works with `buf` tooling.

**Falls short**
- It is RPC-only, with no REST-style resources or HTML, and it is protobuf-first, which not every team wants.
- **Lesson:** this is the best model for Galleon's RPC transport. It is probably adopted or wrapped rather than reinvented.

### Huma
**Does well**
- Its typed handlers use generics and produce OpenAPI 3.1, validation, and problem+json errors without codegen.
- It is router-agnostic.

**Falls short**
- It covers REST APIs only, so it has no HTML, sessions, or jobs.
- Its reflection cost happens at startup, which is fine.
- **Lesson:** it proves FastAPI-style ergonomics work in Go today.

### Goa
**Does well**
- It is the purest design-first tool. One DSL produces HTTP, gRPC, docs, and clients.

**Falls short**
- The DSL is a second language to learn, there is a lot of generated code, and the iteration loop is slow.
- **Lesson:** contract-first is valuable, but the contract should be ordinary Go, not a DSL.

### go-zero and Kratos
**Does well**
- Resilience is built in (rate limiting, circuit breaking, adaptive load shedding, timeouts), along with service discovery and config centers.
- They have strong codegen and opinionated project layouts, and are proven at very large scale in China.

**Falls short**
- They use their own IDL (go-zero) or heavy protobuf-everything (Kratos).
- They have no full-stack or frontend story.
- Their documentation and community are mostly Chinese-language, and they are opinionated in ways that clash with Western Go culture.
- **Lesson:** the internal-services half of "complete" already exists in these. Copy their resilience feature list.

### go-kit
**Does well**
- Its layering is correct (transport, endpoint, service), and its middlewares cover observability and resilience.

**Falls short**
- It is extremely verbose and in maintenance mode.
- **Lesson:** correct abstractions that are painful to type lose to ergonomic ones.

### Encore
**Does well**
- The app's own code is the source of truth for infrastructure: databases, pub/sub, cron, secrets, and caches are declared in Go.
- It gives you local dev with tracing, generated clients, and a service catalog.
- It is the most complete vision in Go, and it handles multi-node by construction.

**Falls short**
- You need its custom toolchain and parser, the `encore` CLI, and conventions it enforces.
- Escaping it means rewriting, and it has no frontend.
- The open-source framework is tied to a commercial platform.
- **Lesson:** "the app describes itself" is enormously powerful, but it should come from standard Go and an optional analyzer, not a mandatory toolchain.

---

## 3. Prior art from other languages, one by one

### Rails
**Does well**
- Its productivity is unmatched. Generators, conventions, migrations, and testing all ship together.
- With Rails 8, the Solid Queue, Solid Cache, and Solid Cable defaults run on the database, so you need no Redis. Hotwire gives interactive UIs without an SPA. Kamal handles deploys.

**Falls short**
- Its runtime magic (metaprogramming, autoloading) makes large codebases hard to navigate.
- It has no typed contracts, and performance and memory are high.
- Multi-process is handled by Puma workers, but cross-node concerns (ActionCable, locks) need adapters.
- **Lesson:** the *experience* is the goal. The DB-backed infra trick is gold for replication: one shared Postgres gives you queues, cache, pub/sub, and locks across nodes.

### Django
**Does well**
- Models are the single source of truth, the admin is free, security defaults are strong, and the docs are excellent.
- Its "apps" make a good modularity unit.

**Falls short**
- It is synchronous-first, and async support is still patchy.
- Realtime needs Channels, background jobs historically needed Celery (Django 6 added a built-in tasks interface), and APIs need DRF.
- It has no contracts without add-ons.
- **Lesson:** an admin derived from models wins internal tools. Plan async and realtime from day one rather than bolting them on.

### FastAPI
**Does well**
- The signature is the contract. Docs and validation come for free, and parameter-based DI is elegant.

**Falls short**
- It covers APIs only, a project's structure is left entirely to you, and large apps get messy.
- Its runtime type introspection costs performance.
- **Lesson:** steal the handler ergonomics, and give large apps a structure FastAPI doesn't.

### Laravel
**Does well**
- It has a first-party answer for nearly everything, a consistent API across all of them, and great docs.
- Its queues and scheduler are replication-aware: `onOneServer()` and `withoutOverlapping()` use atomic locks so cron doesn't double-run.
- Telescope and Horizon give you observability.

**Falls short**
- Facades and global helpers hide dependencies, and runtime magic makes static analysis hard.
- PHP's per-request process model shapes everything, and Octane retrofits long-running workers.
- The commercial ecosystem is part of the value.
- **Lesson:** this is the scope target. The `onOneServer()` style of replication primitive should be built into every Galleon component.

### Symfony
**Does well**
- It is decoupled components plus a kernel, and the components are used far beyond Symfony itself.
- Messenger gives one bus abstraction for sync and async work.

**Falls short**
- Its configuration has been heavy (YAML, XML, attributes), and it is less beginner-friendly than Laravel.
- **Lesson:** this is the structural model: every Galleon component works as a standalone Go module.

### Spring Boot
**Does well**
- It is complete, auto-configured, and production ready (Actuator).
- It has a huge set of integrations and Spring Cloud for distributed concerns: config server, discovery, circuit breakers, and distributed locks.

**Falls short**
- Annotation and reflection magic, slow startup, and high memory use (GraalVM native mitigates but complicates).
- Failures in auto-configuration are hard to debug.
- **Lesson:** Actuator-level operability and override-able auto-configuration are worth having. Do the wiring at compile time instead of runtime reflection.

### ASP.NET Core
**Does well**
- It scales from minimal APIs to full MVC on one pipeline, and its performance is top tier.
- DI, options, and config are built in.
- SignalR (with backplanes for multi-node), gRPC, and Blazor are first party.
- Source generators give AOT and trimming, and Data Protection keys can be shared across nodes.

**Falls short**
- It has a lot of ceremony in places, and the "many ways to do it" problem (MVC, Razor Pages, Minimal APIs, Blazor) confuses newcomers.
- **Lesson:** this is the closest overall model for "complete and efficient". Its multi-node pieces are explicit pluggable backplanes, a pattern Galleon can generalize.

### Phoenix
**Does well**
- Realtime and distribution are native. PubSub and Presence work across a cluster with no extra infra.
- LiveView removes most JS, and contexts give large apps a domain structure.

**Falls short**
- These strengths depend on the BEAM, which Go doesn't have. The Elixir talent pool is small.
- LiveView needs a persistent connection per user, so it suffers on bad networks.
- **Lesson:** Go has goroutines and can ship a clustering layer (membership plus PubSub) to get a lot of this. This is the replication gold standard to aim at.

### Express, Fastify, and Hono
**Do well**
- Express is simple and has a massive ecosystem.
- Fastify has schema-compiled serialization and encapsulated plugins.
- Hono runs anywhere and infers typed clients.

**Fall short**
- Express has no structure.
- Fastify and Hono are API-focused.
- Node's single thread forces clustering via processes, and cross-process state needs Redis.
- **Lesson:** use precompiled, schema-driven serialization. Encapsulated plugins mean every module has its own scoped middleware and dependencies.

### NestJS
**Does well**
- It has structure for big teams, a clear lifecycle vocabulary, and one programming model for HTTP and message transports.

**Falls short**
- Its decorator and reflection heaviness and Angular-style boilerplate are disliked by many.
- **Lesson:** the "same handler model over HTTP, gRPC, and queues" idea is great.

### Next.js and the JS meta-frameworks
**Do well**
- They set the expectations for full-stack DX: routing, data loading, streaming SSR, server actions, and instant HMR.

**Fall short**
- Their complexity is high (caching semantics, server/client boundaries), and they are tied to a JS runtime.
- Self-hosting multi-node needs a shared cache handler.
- They churn a lot between major versions.
- **Lesson:** Galleon needs a stable seam to these, typed clients plus SSR integration, rather than competing with them head-on. It can also offer a Go-native server-driven UI option.

### tRPC
**Does well**
- Server changes break the client at compile time with zero ceremony.

**Falls short**
- It only works with TypeScript on both ends, so it's useless for mobile or other languages.
- **Lesson:** aim for tRPC's feel, but generate for TS, Swift, Kotlin, and Dart (via protobuf or OpenAPI) so mobile gets it too.

### Axum and Actix
**Do well**
- Extractors are the cleanest binding model in any language. `tower` middleware works across server, client, and gRPC (`tonic`).
- Performance is top tier.

**Fall short**
- They are bring-your-own for everything above HTTP.
- Compile errors from extractor and trait mismatches can be opaque.
- **Lesson:** use extractors (via Go generics) and one middleware abstraction shared by server and client.

---

## 4. Patterns across everything

### What the successful ones get right
1. **One obvious way.** Rails, Laravel, and Django win partly by deciding for you.
2. **Error-returning handlers with a central error policy** (Echo, ASP.NET, Axum, FastAPI).
3. **Contracts derived from code** rather than written twice (FastAPI, Huma, tRPC, Encore).
4. **First-party boring stuff** (Laravel, Rails, Spring): jobs, mail, cron, auth, and caching that all match.
5. **Operability out of the box** (Spring Actuator, Telescope, LiveDashboard, Encore's tracing).
6. **A shared foundation interface** (`http.Handler`, `tower::Service`, Rack, Plug) that keeps the ecosystem interoperable.

### What keeps going wrong
1. **Runtime magic.** Reflection, global state, and facades make code hard to read, slow to start, and fail late.
2. **A god-object context** (Gin, Fiber) instead of typed inputs.
3. **Owning the frontend toolchain** (Buffalo) and drowning in JS churn.
4. **Leaving the standard interface** (Fiber, gRPC-Go's separate server) and fracturing the ecosystem.
5. **Bus factor.** Gorilla, Buffalo, and Revel all stalled when their maintainers left.
6. **Replication as an afterthought.** Sessions in memory, cron running on every node, WebSockets that only reach clients on the same node, and rate limits per process. Teams find out in production.
7. **API vs full-stack split.** You get either contracts or HTML productivity, never both in one framework.

### Replication, specifically (your new requirement)

Approaches that exist today, from weakest to strongest:

| Approach | Example | Cost |
|---|---|---|
| Stateless and hope | Gin, Chi, Express | Every team rediscovers the problems |
| Plug in Redis for everything | Laravel, NestJS, Socket.IO adapters | Extra infra, and correctness depends on configuration |
| Database as the coordinator | Rails 8 Solid*, River (Go), Oban (Elixir) | Simple, transactional, but needs a DB everyone shares |
| Pluggable backplanes per component | ASP.NET SignalR, Data Protection | Explicit and swappable, but configured per component |
| Native clustering | Phoenix PubSub and Presence over distributed Erlang | Best experience, but needs a runtime with clustering built in |
| Platform provisions it | Encore, managed platforms | Great until you leave the platform |

**Implication (to argue about later):** Galleon likely needs **one coordination abstraction** (membership, pub/sub, locks, leader election, shared KV) that every component uses. It would come with three backends: an in-process one for dev and single-node, a database one (Postgres) for most apps, and a dedicated one (Redis, NATS, or etcd) for scale. Multi-process on one machine (prefork / `SO_REUSEPORT`, or a supervisor) would be the same problem as multi-node, just with a cheaper transport.

---

## Next

1. Define, exhaustively, every component a framework must have an answer for to be truly complete.
2. Combine those lessons with that list to discuss your desires and argue about their merit.
3. Only then, the architecture.
