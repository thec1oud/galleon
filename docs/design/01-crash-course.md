# Galleon, Part 1: Crash Course on the Go Web Landscape

Stages covered here: (1) frameworks vs libraries, (2) crash courses on the Go prior art, (3) crash courses on the best prior art from other languages, with the lesson each one teaches Galleon.
Strengths and shortcomings come next, once you've read this.

---

## 1. Frameworks vs libraries, in Go terms

### The one-line definition

- **Library:** *you* call *it*. You own `main`, the control flow, and the wiring. A router, a validator, a logger.
- **Framework:** *it* calls *you*. It owns the lifecycle (boot, config, routing, request handling, shutdown) and you fill in the slots it defines. This is "inversion of control."

The practical test: **can you delete it and keep your program's shape?** Removing a library means replacing some calls. Removing a framework means rewriting the skeleton.

### Why this distinction is louder in Go than elsewhere

Go has an unusually strong standard library for the web (`net/http`, `html/template`, `encoding/json`, `database/sql`, `log/slog`, `context`, `testing/httptest`). That shaped a community culture:

- **"Just use the stdlib"** is a respected default, not a beginner's position.
- Frameworks are often viewed with suspicion: "magic", reflection, hidden globals, and incompatible handler signatures are seen as costs.
- The winning tools tend to be **thin and composable** (Chi, sqlc, templ) rather than all-in-one (Rails-style).
- Yet teams building large apps keep re-assembling the same 15 libraries by hand, which is exactly the gap a good framework fills.

### The spectrum

Think of it as a ladder, not a binary:

| Rung | What it gives you | Examples |
|---|---|---|
| 0. Stdlib | HTTP server, basic mux, templates, JSON | `net/http`, `http.ServeMux` |
| 1. Router | Fast path matching, params, groups | `httprouter`, `chi`, `gorilla/mux` |
| 2. Micro-framework | Router + custom context + binding + middleware + error handling | Gin, Echo, Fiber |
| 3. API / service framework | Schema-first contracts, codegen, OpenAPI/protobuf, RPC | gRPC-Go, Connect, Huma, Goa, go-zero, Kratos |
| 4. Full-stack framework | MVC, ORM, migrations, templates, assets, CLI, generators, sessions, jobs | Beego, Buffalo, Revel, Goravel |
| 5. Platform | Framework + infra provisioning + deploy + observability | Encore |

Most of the Go community lives on rungs 0 to 2 and glues the rest together with libraries.

The same ladder exists in every language, which makes it a useful lens:

| Rung | Go | Other languages |
|---|---|---|
| 0. Stdlib / server interface | `net/http` | Node `http`, Python WSGI/ASGI, Ruby Rack, Java Servlet, Rust `hyper` |
| 1. Router | chi, httprouter | `find-my-way` (Node), Werkzeug routing |
| 2. Micro-framework | Gin, Echo, Fiber | Express, Fastify, Hono, Flask, Sinatra, Axum, Actix Web, Ktor |
| 3. API / service framework | gRPC-Go, Connect, Huma, go-zero | FastAPI, NestJS, tRPC, Spring WebFlux |
| 4. Full-stack framework | Beego, Buffalo, Goravel | Rails, Django, Laravel, Phoenix, Spring Boot, ASP.NET Core, Next.js, SvelteKit, AdonisJS |
| 5. Platform | Encore | Vercel + Next.js, Laravel Cloud/Forge/Vapor, Firebase |

The big difference: in Ruby, PHP, Python, Elixir, Java and C#, **rung 4 is where most of the industry lives**. In Go, it's nearly empty. Understanding why is half the design problem.

### Framework vs library, beyond Go

Every ecosystem has had the same argument; the outcomes are instructive:

- **Ruby:** Rails won so decisively that "Ruby developer" nearly means "Rails developer." Sinatra stayed the micro alternative. Lesson: a great full-stack framework can define a language's identity.
- **Python:** Split three ways. Django (batteries included), Flask (micro, assemble yourself), FastAPI (typed API framework). All three thrive because they serve different jobs.
- **PHP:** Laravel and Symfony dominate. Symfony is a set of decoupled components that Laravel itself builds on. Lesson: a framework can be **built from independently usable libraries**, getting both models at once.
- **Java / C#:** Spring Boot and ASP.NET Core are heavy, DI-centric, enterprise-grade and hugely productive at scale. Lesson: completeness plus conventions plus tooling wins large teams.
- **JavaScript:** Started micro (Express), then went structured (NestJS) and then full-stack meta-frameworks (Next.js, Remix/React Router, SvelteKit, Nuxt) that blur frontend and backend. Lesson: the frontend story decides full-stack adoption.
- **Rust:** Mostly micro (Axum, Actix Web) on a shared foundation (`tokio`, `hyper`, `tower`). Lesson: a **shared middleware abstraction** (`tower::Service`) lets the whole ecosystem interoperate, just like `http.Handler` in Go.
- **Elixir:** Phoenix is the one framework, and it leans on the runtime (BEAM) for realtime and fault tolerance. Lesson: design around your language's superpower, not around another language's framework.

### The layers any serious web stack needs

This is the checklist we'll score prior art against later, and the map for Galleon's architecture:

1. **Transport:** HTTP/1.1, HTTP/2, HTTP/3, TLS, WebSockets, SSE, graceful shutdown, and **gRPC to external clients** (e.g. mobile apps that want compact binary payloads and streaming), not only gRPC between services.
2. **Routing:** matching, params, groups, mounting, reverse routing (URL generation).
3. **Middleware:** composition model, ordering, per-route vs global.
4. **Request binding and validation:** path/query/header/body/form/multipart into typed structs.
5. **Response rendering:** JSON, HTML, streaming, content negotiation.
6. **Errors:** a single error model, mapping to status codes and problem+json.
7. **Contracts:** OpenAPI / protobuf generation, typed clients.
8. **Dependency wiring and lifecycle:** DI, config, startup/shutdown hooks, modules.
9. **Data:** DB access, migrations, transactions, caching.
10. **Auth:** sessions, cookies, CSRF, OAuth/OIDC, API keys, authorization policy.
11. **Background work:** jobs, queues, cron, retries.
12. **Frontend (full-stack):** templating, components, assets, hot reload, islands/htmx, SSR.
13. **Observability:** structured logs, metrics, tracing (OpenTelemetry), health checks.
14. **Testing:** handler tests, fakes, fixtures, integration harness.
15. **Tooling:** CLI, scaffolding, codegen, dev server.
16. **Inter-service:** RPC, service discovery, resilience (timeouts, retries, circuit breakers).
17. **Replication and distribution (cross-cutting):** running as many processes on one machine and many nodes across machines, out of the box. Every other layer must be replication-aware: sessions, caches, rate limits, WebSocket/realtime fan-out, jobs and cron (no double runs), migrations (run once), config, locks, and graceful rolling restarts.

### The Go-specific design tensions

Keep these in mind; every framework below picks a side on each:

- **`http.Handler` compatibility vs a custom handler signature.** Compatibility buys the whole ecosystem; custom signatures buy ergonomics (returning errors, typed context).
- **`net/http` vs `fasthttp`.** fasthttp is faster in microbenchmarks but is a different world (no HTTP/2, incompatible types).
- **Reflection vs code generation vs generics.** Reflection is convenient and slow-ish with runtime errors; codegen is fast and type-safe but adds a build step; generics (since Go 1.18) are a newer middle path.
- **Explicit wiring vs "magic".** Go developers forgive verbosity much more than surprise.
- **Monolithic module vs many small modules.** One import is easy; many lets people take only what they need.

---

## 2. Crash courses on the prior art

I picked the projects the community actually uses most, plus a few that matter as design references even if their usage is smaller. Star counts and versions move; treat the "standing" notes as approximate.

### 2.1 `net/http` + `ServeMux` (the baseline)

**What it is:** The standard library HTTP server and router. Since **Go 1.22** the built-in `ServeMux` supports method matching and path wildcards, which removed the main reason many people reached for a third-party router.

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    json.NewEncoder(w).Encode(map[string]string{"id": id})
})
http.ListenAndServe(":8080", mux)
```

**Core ideas:**
- Everything is `http.Handler` (`ServeHTTP(w, r)`). This is the lingua franca of Go web.
- Middleware is just `func(http.Handler) http.Handler`.
- Request-scoped data rides on `context.Context` (`r.Context()`).
- HTTP/2 built in with TLS; graceful shutdown via `Server.Shutdown`.

**Standing:** Universal. Every other `net/http`-based tool is a layer on top of this.

### 2.2 Gin

**What it is:** The most popular Go web framework by stars and usage. A micro-framework built on a radix-tree router derived from `httprouter`.

```go
r := gin.Default() // includes Logger and Recovery middleware
r.GET("/users/:id", func(c *gin.Context) {
    var q struct{ Verbose bool `form:"verbose"` }
    if err := c.ShouldBindQuery(&q); err != nil {
        c.JSON(400, gin.H{"error": err.Error()}); return
    }
    c.JSON(200, gin.H{"id": c.Param("id")})
})
r.Run(":8080")
```

**Core ideas:**
- **`*gin.Context`** wraps request and response and carries params, bound data, and a key/value store. Pooled with `sync.Pool` for low allocation.
- **Binding** via struct tags (`json`, `form`, `uri`, `binding:"required"`) with `go-playground/validator` underneath.
- **Route groups** with group-level middleware; middleware calls `c.Next()` / `c.Abort()`.
- Handlers return nothing; you write the response and `return`.

**Standing:** Largest community, most tutorials, huge middleware collection (`gin-contrib`). The default pick in many companies and especially popular in China.

### 2.3 Echo

**What it is:** Gin's closest rival, from LabStack. Similar shape, slightly more opinionated about errors and more batteries.

```go
e := echo.New()
e.Use(middleware.Logger(), middleware.Recover())
e.GET("/users/:id", func(c echo.Context) error {
    u := new(User)
    if err := c.Bind(u); err != nil { return echo.ErrBadRequest }
    return c.JSON(http.StatusOK, u)
})
e.Logger.Fatal(e.Start(":8080"))
```

**Core ideas:**
- **Handlers return `error`**, and a central `HTTPErrorHandler` turns errors into responses. This is a big ergonomic win over Gin.
- `echo.Context` is an interface (mockable, extensible by embedding).
- Built-in middleware set is broad: CORS, CSRF, JWT, gzip, rate limit, body limit, secure headers, static files.
- Auto TLS via Let's Encrypt, HTTP/2, a renderer interface for templates.

**Standing:** Second-tier in popularity to Gin but well regarded for API design and docs.

### 2.4 Fiber

**What it is:** An Express.js-inspired framework built on **`fasthttp`** instead of `net/http`. Targets people coming from Node and people chasing benchmark numbers.

```go
app := fiber.New()
app.Get("/users/:id", func(c *fiber.Ctx) error {
    return c.JSON(fiber.Map{"id": c.Params("id")})
})
app.Listen(":8080")
```

**Core ideas:**
- **fasthttp underneath:** aggressive object reuse, near-zero allocation, very high throughput in benchmarks.
- Express-style API (`app.Get`, `c.Params`, `c.SendString`, `app.Static`).
- Handlers return `error`; large official middleware and template-engine set.
- **The catch:** values from `*fiber.Ctx` are only valid during the handler because buffers are reused. Copy anything you keep. Not `http.Handler` compatible (adapters exist, with overhead). No native HTTP/2.

**Standing:** Very popular (star count comparable to Gin), especially with newcomers. Polarizing among experienced Go developers because it leaves the `net/http` ecosystem.

### 2.5 Chi

**What it is:** A lightweight, idiomatic router that stays 100% `net/http` compatible. The "stdlib purist's router."

```go
r := chi.NewRouter()
r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
r.Route("/users", func(r chi.Router) {
    r.Get("/{id}", getUser)        // getUser is a plain http.HandlerFunc
    r.With(adminOnly).Delete("/{id}", deleteUser)
})
http.ListenAndServe(":8080", r)
```

**Core ideas:**
- Handlers are plain `http.HandlerFunc`; middleware is plain `func(http.Handler) http.Handler`. Anything from the ecosystem plugs in.
- Composable sub-routers (`Route`, `Mount`, `Group`, `With`) for large route trees.
- Small, stable, no external deps in the core. Optional companions: `render`, `cors`, `httprate`, `jwtauth`.
- No binding, validation, or error model. You bring those.

**Standing:** The favourite of teams that want structure without leaving the stdlib. Used widely in production services. Its value proposition partly overlaps with Go 1.22's improved `ServeMux`.

### 2.6 Gorilla toolkit (historical reference)

**What it is:** `gorilla/mux`, `gorilla/websocket`, `gorilla/sessions`, `gorilla/csrf`, etc. For years the default toolkit. Archived in late 2022, then revived by new maintainers in 2023.

**Why it matters:** It's the clearest example of the **"toolkit of libraries"** model in Go, and a cautionary tale about ecosystem dependence on a few volunteers. `gorilla/websocket` is still very widely used.

### 2.7 Full-stack frameworks

These try to be "Rails/Laravel/Django for Go." None has reached the dominance those have in their languages, which is itself a key data point for Galleon.

#### Beego
- **What:** A full MVC framework with router, ORM, cache, sessions, config, logging, i18n, admin monitoring, and the `bee` CLI for scaffolding and hot reload.
- **Ideas:** Controller structs with methods (`Get()`, `Post()`), annotation-based routing via comments, its own ORM.
- **Standing:** Most popular full-stack Go framework, especially in China. Seen as dated and heavy by parts of the wider community.

#### Buffalo
- **What:** "Rails for Go." Generators, Pop ORM + Fizz migrations, Plush templates, asset pipeline (webpack), background workers, `buffalo dev` hot reload, resources (REST controllers).
- **Ideas:** Productivity first, conventions over configuration, the whole stack from DB to JS.
- **Standing:** Was the flagship attempt at a Go Rails. Development has wound down and it is effectively unmaintained now. A valuable lesson in how hard maintaining the full stack (especially the JS asset pipeline) is.

#### Revel
- **What:** An early (2012+) high-productivity framework inspired by Play! (Scala/Java). Hot code reload, routes file, controllers, interceptors, templates, testing harness.
- **Standing:** Largely dormant. Historically important; fought Go idioms with a lot of magic.

#### Goravel (worth knowing)
- **What:** A newer framework explicitly modelled on Laravel: service container, facades, Artisan-style CLI, ORM (on GORM), queues, events, mail, scheduling, auth, validation.
- **Standing:** Smaller but active. Shows ongoing demand for a "batteries-included" Go experience.

#### The modern "assemble your own full stack" pattern
Many teams now build full-stack Go apps without a full-stack framework, using:
- **templ** (type-safe, compiled HTML components) + **htmx** (server-driven interactivity), sometimes Alpine.js. Sometimes called the "GOTH" stack.
- **sqlc** (SQL to typed Go code) or **ent** / **GORM** for data.
- **air** for hot reload, **goose** / **atlas** / **golang-migrate** for migrations.
- **SCS** or `gorilla/sessions` for sessions.

This pattern matters: it's what Galleon would be competing with for full-stack work.

### 2.8 Service and RPC frameworks (internal services)

#### gRPC-Go
- **What:** Google's RPC framework. Protobuf contracts, codegen for servers and clients, HTTP/2 streaming (unary, server, client, bidi), interceptors, deadlines, load balancing, health checks, reflection.
- **Standing:** The default for internal service-to-service calls in Go shops. Downsides: needs HTTP/2 end to end, browsers need a proxy (gRPC-Web), and its own server type separate from `net/http`.

#### Connect (connectrpc, by Buf)
- **What:** A protobuf RPC framework that speaks **three protocols on one handler**: gRPC, gRPC-Web, and its own simple Connect protocol (plain HTTP/1.1 or 2, JSON or binary, curl-friendly).
- **Ideas:** Generated handlers are plain `http.Handler`s, so they mount on any mux next to normal routes. Generic request/response types (`connect.Request[T]`).
- **Standing:** Fast-growing, increasingly seen as the modern replacement for raw gRPC-Go when you want both browser and service clients.

#### Huma
- **What:** An OpenAPI-first REST framework that sits **on top of** your router (chi, Gin, Echo, Fiber, stdlib).
- **Ideas:** You declare operations with typed input/output structs using generics; Huma generates OpenAPI 3.1 and JSON Schema, validates requests, and returns RFC 9457 problem+json errors.
- **Standing:** Smaller but influential. Shows how generics can give typed handlers and docs without codegen.

#### Goa
- **What:** Design-first: you write a Go DSL describing your API, and Goa generates transport code (HTTP and gRPC), OpenAPI, and clients.
- **Standing:** Niche but mature. The extreme end of "contract first."

#### go-zero and Kratos
- **go-zero:** Microservice framework with its own `.api` IDL and `goctl` codegen; built-in rate limiting, circuit breaking, load shedding, caching. Very popular in China.
- **Kratos** (from Bilibili): Protobuf-first microservice framework with layered project templates (DDD-ish), Wire for DI, transports for HTTP and gRPC, registry/config plugins.
- **Why they matter:** They are the closest existing things to a complete "internal services" framework, with resilience built in.

#### go-kit
- **What:** A toolkit (explicitly not a framework) for microservices: endpoints, transports, middlewares for logging, metrics, tracing, circuit breaking.
- **Standing:** Influential in 2015 to 2019, now in maintenance mode. Famous for being correct and very verbose.

### 2.9 Platform-level: Encore

- **What:** A backend framework plus platform. You annotate Go functions (`//encore:api`), and Encore's tooling parses your code to understand services, APIs, databases, pub/sub, cron, and secrets, then provisions infrastructure, generates clients and docs, and gives you local dev with tracing.
- **Ideas:** Static analysis of the app as the source of truth for infra. The "app knows its own architecture."
- **Standing:** Growing. The most ambitious "complete through and through" vision in Go today, at the cost of a custom toolchain and parser.

### 2.10 Supporting libraries worth naming (not frameworks)

These come up constantly and Galleon will either integrate or replace them:

| Concern | Common choices |
|---|---|
| Validation | `go-playground/validator` |
| DI | `google/wire` (codegen), `uber-go/fx` and `dig` (runtime) |
| Config | `spf13/viper`, `kelseyhightower/envconfig`, `koanf` |
| Logging | `log/slog` (stdlib), `zap`, `zerolog` |
| Observability | OpenTelemetry Go, Prometheus client |
| Data | `database/sql`, `pgx`, `sqlc`, `ent`, `GORM`, `bun` |
| Migrations | `goose`, `golang-migrate`, `atlas` |
| Jobs | `asynq`, `river` (Postgres-based) |
| Templates | `html/template`, `templ` |
| WebSockets | `gorilla/websocket`, `coder/websocket` |
| CLI | `cobra`, `urfave/cli` |

---

## 3. Crash courses beyond Go

Each entry ends with **"Lesson for Galleon"**: the idea worth stealing (or avoiding).

### 3.1 Ruby on Rails (Ruby)

**What it is:** The archetypal full-stack framework (2004). MVC, ActiveRecord ORM, migrations, generators, routing DSL, mailers, background jobs (ActiveJob), caching, WebSockets (ActionCable), file uploads (ActiveStorage), and since Rails 7/8 a "no-build" frontend story: Hotwire (Turbo + Stimulus), import maps, Solid Queue/Cache/Cable (DB-backed infra), and Kamal for deploys.

```ruby
# config/routes.rb
resources :users
# app/controllers/users_controller.rb
class UsersController < ApplicationController
  def show
    @user = User.find(params[:id])   # renders app/views/users/show.html.erb
  end
end
```

**Core ideas:** Convention over configuration (names imply wiring), "The Rails Way" as a single blessed path, generators that create a working slice in one command, a "one-person framework" philosophy.

**Lesson for Galleon:** Conventions and generators are the biggest productivity lever there is. Rails 8 also shows you can ship queues, caching and realtime **on the database you already have**, removing Redis/Kafka for most apps. The cost to avoid: implicit magic (metaprogramming, autoloading) that Go developers won't accept.

### 3.2 Django (Python)

**What it is:** "The web framework for perfectionists with deadlines." ORM, migrations (auto-generated from models), the famous **auto-generated admin**, forms, auth and permissions, sessions, templates, i18n, security middleware on by default. Django REST Framework adds APIs.

**Core ideas:** Project made of reusable "apps"; models are the single source of truth (schema, migrations, admin, forms all derive from them); secure defaults (CSRF, XSS escaping, clickjacking protection).

**Lesson for Galleon:** **Derive everything from one schema definition.** The admin alone wins Django countless internal tools. Pluggable "apps" give a modularity model for large codebases. Secure by default should be non-negotiable.

### 3.3 FastAPI (Python)

**What it is:** A typed API framework built on Starlette (ASGI) and Pydantic. You write a function with type hints; FastAPI validates input, serializes output, and generates OpenAPI docs with Swagger UI.

```python
@app.get("/users/{id}")
async def get_user(id: int, verbose: bool = False) -> User:
    return await repo.get(id)
```

**Core ideas:** The **function signature is the contract**. Dependency injection through parameters (`Depends(get_db)`). Docs are a free by-product, never written by hand.

**Lesson for Galleon:** The most loved API ergonomics of the last decade. Go generics can now express typed handlers like `func(ctx, In) (Out, error)` and get the same benefits (this is what Huma does). Galleon's handler signature should feel like FastAPI.

### 3.4 Laravel (PHP)

**What it is:** The most complete batteries-included framework in wide use. Service container (DI), facades, Eloquent ORM, migrations and seeders, Artisan CLI, queues, events, scheduler, mail, notifications, broadcasting, auth scaffolding, validation, Blade templates, and first-party ecosystem products (Horizon, Telescope, Sanctum, Livewire, Inertia, Octane, Reverb).

**Core ideas:** Developer happiness, expressive APIs, a first-party answer to every question, and an ecosystem company backing it.

**Lesson for Galleon:** "Complete through and through" looks like Laravel: **first-party, consistent answers** for queues, scheduling, mail, notifications, observability (Telescope), and realtime. Livewire/Inertia show two credible ways to do full-stack interactivity without a separate SPA. Facades (global static access) are what to avoid in Go.

### 3.5 Symfony (PHP)

**What it is:** A full framework assembled from ~50 decoupled, independently usable components (HttpFoundation, Routing, DependencyInjection, Console, EventDispatcher, Messenger…). Laravel and Drupal use Symfony components.

**Lesson for Galleon:** The **"framework made of libraries"** model resolves Go's framework-vs-library tension directly: every piece usable alone, and a kernel that composes them by convention.

### 3.6 Spring Boot (Java/Kotlin)

**What it is:** The enterprise standard. Inversion-of-control container, annotation-driven controllers, auto-configuration ("add a dependency and it configures itself"), starters, Spring Data, Spring Security, Actuator (health, metrics, info endpoints), profiles, and huge integration coverage. Spring Native / GraalVM for fast startup.

**Core ideas:** DI everywhere, auto-configuration with override, production readiness built in.

**Lesson for Galleon:** **Actuator-style operability** (health, readiness, metrics, config introspection) out of the box is exactly what internal services need. Auto-configuration is powerful, but heavy annotation/reflection magic and slow startup are the costs to avoid; Go can achieve similar wiring at compile time.

### 3.7 ASP.NET Core (C#)

**What it is:** Microsoft's cross-platform web stack. Very fast (Kestrel server tops benchmarks), with both **Minimal APIs** (Express-like) and MVC controllers, a built-in DI container, a configuration/options system, middleware pipeline, Identity, SignalR (realtime), gRPC, Blazor (C# in the browser or server-rendered components), and source generators for AOT.

```csharp
app.MapGet("/users/{id}", async (int id, IUserRepo repo) =>
    await repo.Get(id) is User u ? Results.Ok(u) : Results.NotFound());
```

**Lesson for Galleon:** Proof that **one framework can scale from a 5-line service to a huge enterprise app** without switching tools: minimal APIs and full MVC share the same pipeline, DI and config. Also shows that "complete" and "fastest" are not contradictory. Its move to compile-time source generators mirrors the direction Go should take.

### 3.8 Phoenix (Elixir)

**What it is:** A full-stack framework on the BEAM VM. Ecto (data mapping, changesets, migrations), Channels and PubSub (realtime), **LiveView** (server-rendered interactive UIs over WebSockets, no JS framework), Presence, generators, LiveDashboard.

**Core ideas:** Plug pipelines (composable request transforms), contexts (domain boundaries), lean on lightweight processes for millions of connections.

**Lesson for Galleon:** LiveView redefined server-driven UI. Go's goroutines make a LiveView-style model plausible. **Contexts** are a good convention for structuring large apps by domain. Ecto **changesets** are an excellent model for validation and casting separate from the data struct.

### 3.9 Express, Fastify, Hono (Node / JS runtimes)

- **Express:** The minimalist original. `(req, res, next)` middleware. Huge ecosystem, little structure. The model Fiber copies.
- **Fastify:** Express's faster successor: JSON Schema per route for validation and **precompiled serialization**, an encapsulated plugin system, built-in logging (pino), hooks lifecycle.
- **Hono:** Tiny, Web-standards based (`Request`/`Response`), runs everywhere (Node, Bun, Deno, Cloudflare Workers), typed RPC client generated from routes.

**Lesson for Galleon:** Fastify's **schema-compiled serializers** and **encapsulated plugins** are worth copying. Hono shows the power of a **typed client inferred from the server** with zero codegen. Express shows how an unstructured framework produces inconsistent large codebases.

### 3.10 NestJS (TypeScript)

**What it is:** An Angular-inspired, structured backend framework: modules, providers, controllers, DI, decorators, guards, interceptors, pipes, exception filters, plus first-class microservice transports (gRPC, NATS, Kafka, Redis) and GraphQL.

**Lesson for Galleon:** A clear **request lifecycle vocabulary** (guard → interceptor → pipe → handler → filter) makes large teams consistent. Same programming model for HTTP and message-based microservices. The decorator/reflection-heavy style is the part not to copy.

### 3.11 Next.js, Remix / React Router, SvelteKit (JS meta-frameworks)

**What they are:** Full-stack frameworks where the frontend framework is the center: file-based routing, server-side rendering, streaming, server components / server actions, loaders and actions for data, edge deployment, bundling and hot reload built in.

**Lesson for Galleon:** This is where full-stack developer expectations are set today: file-system routing, colocated data loading, type-safe forms, instant hot reload, streaming HTML. Galleon must have a credible answer, either a first-class Go-native UI layer (templ/htmx/LiveView style) or first-class integration with these as frontends (typed clients, SSR proxying), likely both.

### 3.12 tRPC (TypeScript)

**What it is:** End-to-end typed RPC between a TypeScript server and client with no schema or codegen; the client type is inferred from the server router.

**Lesson for Galleon:** The bar for "typed contracts" is now **change the server, see the client break at compile time**. Galleon should generate typed TS and Go clients automatically from its routes.

### 3.13 Axum and Actix Web (Rust)

- **Axum:** Built on `tokio`, `hyper` and **`tower`**. Handlers are plain async functions whose arguments are **extractors** (`Path<T>`, `Json<T>`, `State<T>`), each one parsing part of the request. Middleware is any `tower::Layer`.
- **Actix Web:** Actor-heritage framework, consistently near the top of benchmarks, similar extractor model.

```rust
async fn get_user(Path(id): Path<u64>, State(db): State<Db>) -> Result<Json<User>, AppError> { … }
```

**Lesson for Galleon:** **Extractors** are the cleanest binding model in any language: type-driven, composable, and testable. And a **shared middleware abstraction** (`tower`) used by every framework (HTTP, gRPC via `tonic`, clients) means one timeout/retry/rate-limit layer works everywhere. Galleon's equivalent should be built on `http.Handler` and work for both server and client.

### 3.14 Cross-language takeaways (preview)

The recurring ideas across all of these, which we'll weigh in the next stage:

1. **One source of truth** (Django models, FastAPI signatures, protobuf) from which validation, docs, clients and admin are derived.
2. **Typed handlers and extractors** instead of a god-object context.
3. **A framework built from independently usable components** (Symfony, tower).
4. **First-party answers for the boring 80%** (Laravel, Rails): jobs, mail, scheduling, auth, realtime.
5. **Production readiness built in** (Spring Actuator, Phoenix LiveDashboard, Laravel Telescope).
6. **A credible full-stack UI story** (Hotwire, LiveView, Livewire/Inertia, Next.js).
7. **Scale from tiny to huge without switching tools** (ASP.NET Core).
8. **Compile-time over runtime magic** (ASP.NET source generators, Rust macros), which fits Go's codegen culture.

---

## Quick mental map

```
                 contracts & codegen ▲
                                     │   Goa   Kratos  go-zero    Encore (platform)
                                     │      Connect  gRPC-Go
                                     │   Huma
                                     │
  thin / composable ◄────────────────┼────────────────────────► batteries included
                                     │
           net/http  Chi  Gorilla    │   Gin  Echo  Fiber        Beego  Buffalo  Goravel
                                     │                           Revel
                 runtime & reflection▼
```

Nothing currently sits in the top-right with idiomatic `net/http` compatibility and broad adoption. That empty corner is the interesting one for Galleon.

---

## Next

1. Strengths and shortcomings: what each of these does well, where it falls short, and the patterns across them.
2. Your desires.
3. The architecture.
