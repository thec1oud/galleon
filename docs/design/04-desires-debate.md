# Galleon, Part 4: Your Desires, and Where I Push Back

Builds on parts [1](01-crash-course.md), [2](02-strengths-and-shortcomings.md) and [3](03-complete-component-list.md). Component IDs (J3, K7, O2…) refer to part 3.

## What you said, restated

1. You are building six systems of your own, each standalone and each meant to plug into Galleon (or anything else) through a seam:
   - **dpg**, a migration tool
   - **an ORM**
   - **comms**, all inbound and outbound messaging, probably a separate service, integrating through an outbox
   - **vantage**, enterprise IAM with a new model called CBAC (context-based access control), leaning towards a separate service
   - **billing**, a standalone service integrated through a seam
   - **Themis**, a DSL for defining APIs, with its own generators and validators
2. Galleon must have a seam for each of those **and** let people use other tools in their place.
3. For every component Galleon doesn't bundle, it must offer **the most standard, exhaustive and extensive seam** possible.
4. Galleon must be **correct, complete and ergonomic**.
5. Data-access seams must support mixing read and write transactions. For example, a module may want a specific or random read transaction instead of the primary write transaction.
6. Galleon defines the seams for validation and other API operations, and Themis's Galleon-specific generators fill them in. Galleon may adopt Themis as the default and still let people use something else.

I agree with the direction. Below are the places where I think it either breaks as stated or needs a sharper rule.

---

## Decisions so far

| # | Topic | Decision |
|---|---|---|
| D1 | vantage | **Both**: the service is the source of truth, and an embedded engine decides locally. |
| D2 | CBAC | **A context-resolution stage that runs before RBAC/ABAC.** It determines which operating context a person is in (a domain such as user, org, team or locality) and gates access quickly, with no lookup. It builds on RBAC/ABAC rather than replacing them, takes composable inputs, and is more complex than this summary. |
| D3 | ORM | A full ORM, with raw SQL and a type-safe query builder. **PostgreSQL only** for now. |
| D4 | dpg | **PostgreSQL only.** A [declarative, state-based schema compiler](https://github.com/thec1oud/dpg). |
| D5 | Priorities | **Correct > ergonomic > efficient > complete in core**, with the efficiency budget rule: no ergonomic feature may add per-request reflection or allocation on the hot path unless you opt in. |
| D6 | Frontend | **A battery** with a seam. The default is **templ** components, htmx-style fragment responses, and **Vite** as the asset seam. |
| D7 | Coordination backend | **No default.** It must always be configured explicitly, including `local` for single-node, which is the internal default deployment. |
| D8 | Reads inside a write transaction | **Join** by default. **Independent** only when a module asks for it. |
| D9 | Themis | **Its own language.** It can import (by generating DSL) and export, and it covers events. |
| D10 | Transaction API | Both **begin/commit pairs** and **closures**; see Round 2. |
| D11 | IAM independence | **Galleon must not depend on vantage's shape.** Context resolution and authorization are generic seams that any IAM can fill. vantage is one adapter, and CBAC is still in active development. |
| D12 | Migrations | **dpg's design, including data migrations, is out of scope.** Galleon defines a tool-neutral migration seam: a small required core plus optional capabilities. See Round 3. |

---

## Round 2: my responses to your answers

### Begin/commit pairs plus closures (D10): not a bad idea, with three rules

Having both is fine. pgx already works this way: calling `Begin` on an existing transaction creates a **savepoint**, and that inner `Commit` releases the savepoint without committing the outer transaction. So a function that accepts a transaction can call `Begin`/`Commit` on it safely without knowing whether it's the outermost caller. Pairs are also the only option when a transaction's lifetime isn't lexical: a request-scoped transaction opened in middleware, a streaming response, or a multi-step handler.

It becomes a bad idea only if it's unstructured. The rules that keep it correct:

1. **Ownership.** Only the code that began the root transaction can commit it. A callee that calls `Begin` on a handle it was given gets a nested handle (a savepoint), so its `Commit` can never commit the caller's work. This is enforced by the type, not by convention.
2. **`defer tx.Rollback(ctx)` is always safe.** It is a no-op after a successful `Commit`. That's the standard Go idiom, and it's what makes forgetting to roll back on error impossible.
3. **Leak detection.** In dev and tests, Galleon tracks open transactions. A transaction still open when a request or job ends is reported with the stack trace of where it began. In production it is rolled back and logged.

How the two forms relate: **the closure is sugar over the pair**, so there is one implementation underneath. What the closure adds is **automatic retry** on serialization failures and deadlocks, because only a closure can be re-run. With raw pairs, retry is your responsibility. Galleon can still offer a helper, but it can't retry code it doesn't control. That trade-off belongs in the docs, not hidden.

One note for the ORM: a nested `Begin` that creates a savepoint is pgx behaviour. `database/sql` has no nested transactions. Since your ORM and dpg are Postgres-only, the Galleon data seam should be **pgx-native** (faster too), with a `database/sql` adapter only for other databases.

Pushback 9, point 7 is revised accordingly: closures **and** begin/commit pairs, with retries available only in the closure form.

### CBAC (D2): what it means for Galleon's seams

If CBAC resolves the operating context *before* RBAC/ABAC and does it **without a lookup**, the resolved context has to be **carried in the credential or the session**: signed claims, or a compact, verifiable context token. That gives Galleon three requirements:

1. **A context-resolution stage in the request pipeline** (E2). It runs immediately after authentication and before routing reaches the handler. It produces a typed `OperatingContext` value in the request context.
2. **Operations declare which contexts they accept** in the operation model (Pushback 10). Themis can declare it in the DSL. The gate is then a cheap comparison, decided before any RBAC/ABAC call to vantage.
3. **Context switching is a first-class flow.** For example, a user switches from org A to org B. That issues a new token or session state, and every node must honour it immediately. That falls under multi-node (K3 revocation, O2).

When you're ready, a short spec for CBAC would let me pin these seams down. I'd only need the inputs, the outputs, and how a context is switched or revoked.

### dpg (D4): I had the migration seam wrong

I had assumed versioned migration files ("list pending, apply"). dpg is **declarative**: you describe the target schema, and it plans the difference, classifies each statement (SAFE, CAUTION, DESTRUCTIVE, MANUAL), and blocks destructive changes by default. So the migration seam (J3) has to fit **both** models, because other users will bring goose or golang-migrate (versioned) or Atlas (declarative):

- **Plan:** compute what would change. That fits dpg's offline `plan`/`diff`, and can run in CI.
- **Apply:** run a plan on the primary, **exactly once across the cluster**, under the coordination lock.
- **Verify:** check at boot that the live schema matches what this build expects. That fits `dpg verify`'s drift check, and **gates readiness** (A2), so a node never serves traffic against the wrong schema.
- **Policy:** let deploys refuse plans above a safety level (e.g. block DESTRUCTIVE in automatic deploys).

The tool-neutral seam that replaces this is in Round 3.

### Ergonomics vs efficiency (D5): ergonomics, with an efficiency budget

**My recommendation: ergonomics ranks above efficiency, but with a hard rule:** *no ergonomic feature may add per-request reflection or allocation on the hot path, unless you opt in to it.*

Why that order:
- For most apps, the database and the network dominate request time. A framework that's 20% faster at routing rarely changes a real app's latency, while an awkward API changes every line of code.
- Fiber is the cautionary tale from part 2: efficiency bought at the cost of compatibility and safety.
- Much of the conflict disappears anyway, because Themis generates the decoders, encoders and validators. Generated code is both ergonomic to use and efficient to run. Most of what's left gets solved at boot time (precompiled routes, DI graph, serializers), not per request.

The rule turns "efficient" from a vague value into something reviewable: each component documents its hot-path cost, and benchmarks in CI (R5) catch regressions.

Proposed final order: **correct > ergonomic > efficient (with the budget rule) > complete in core.**

### Frontend default (D6): my suggestion

**Default battery: templ for components, with htmx-style fragment responses, and Vite as the asset seam.**

- **templ** gives type-safe, compiled HTML components. Templates are checked at compile time, it's fast, and it already has an LSP and real adoption.
- **Fragment responses** (htmx/Hotwire style) give interactivity without an SPA. Galleon needs to support partial rendering, out-of-band updates and request headers that say "this is a fragment request", not ship a JS framework.
- **Vite as a seam, not an owned toolchain.** Galleon reads Vite's manifest for fingerprinted asset URLs, proxies its dev server, and embeds the built output in the binary. That keeps the Buffalo lesson in mind: never own the JS toolchain.
- **The SPA and meta-framework path** (React, Svelte, Next.js) uses the same Vite seam, plus typed TS clients from Themis.
- **A LiveView-style server-driven UI** can come later as a separate battery. It needs the realtime and multi-node work (M2, M5) first.

The other option is stdlib `html/template`. It has no dependency, but it isn't type-safe and has no component model. I'd keep it as a supported alternative, not the default.

### Coordination backend with no default (D7): agreed, plus one safeguard

Making it explicit is consistent with "correct". The safeguard: components **declare** what they need from coordination (cluster-wide locks, pub/sub, leader election, shared KV). At boot, Galleon checks the declared needs against the configured backend.
- With `local`, a component that needs cluster-wide guarantees (cron singletons, global rate limits) still works, but Galleon logs clearly that its guarantees are **per process**.
- A setting such as `deployment: single-node` lets Galleon refuse to start in a context that shows more than one instance (for example a Kubernetes Deployment with more than one replica, when it can detect that). It's best-effort, since Galleon can't always know.

Backends: `local`, Postgres, Redis, NATS and etcd, none of them the default.

### Causal tokens (question 9): what's standard, and what they cost

**What's standard:** there is **no IETF or HTTP standard** for read-your-writes tokens. The established practice is an **opaque session or consistency token**:
- Azure Cosmos DB returns a session token in a response header and accepts it back on later requests.
- MongoDB's causally consistent sessions pass the cluster time and operation time between operations.
- Google's Zanzibar uses "zookies", and SpiceDB's equivalent is ZedTokens: opaque tokens carrying a consistency point.

So the standard approach is **an opaque, signed, versioned token**:
- sent in a response **header** for APIs and mobile clients,
- in **gRPC metadata** for RPC,
- optionally in a **cookie** for browsers.

**Performance and efficiency:**

| Step | Cost |
|---|---|
| Capturing the LSN after commit | One extra `pg_current_wal_lsn()` call, which can be pipelined with `COMMIT` in the same round trip using pgx batching, so there's effectively no extra latency. It's only needed for write transactions. |
| Token size | An LSN is 8 bytes. With a version and an HMAC, the token is roughly 40 to 60 bytes base64, sent **only after a write**. |
| Verifying it | An HMAC over a few dozen bytes takes about a microsecond. |
| Picking a replica | Galleon polls each replica's `pg_last_wal_replay_lsn()` in the background (say every 50 to 100 ms) and routes to one that has passed the token's LSN. That's a comparison of two integers per request, with no extra query. |
| When no replica has caught up | Either wait (`WAIT FOR LSN` with a timeout on PostgreSQL 19, or polling before that), or fall back to the primary. That's a per-read policy. Only `ReadYourWrites` reads ever pay this. |

**Compared to the alternatives:** Rails sends **all** of a user's reads to the primary for 2 seconds after any write, which adds primary load whether or not replicas have caught up. Laravel stays sticky only within one request. The token approach sends a read to the primary **only when a replica actually hasn't caught up**, so it beats both on load *and* correctness.

**Security:** a forged token can't leak data. At worst it can force waits or primary reads, which is why it's signed and why its LSN is clamped to the primary's current LSN.

### Themis (D9): what changes

- **Import by generating DSL** gives teams a migration path from existing OpenAPI and proto files. Galleon's operation model stays the single runtime truth.
- **Events in Themis** mean the operation model must cover **event operations** (publish, subscribe, with topic, schema, delivery guarantees). Those plug into the outbox, inbox and broker consumers (J9, L4) through the integration kit (Pushback 3). That makes one model for requests, RPC and events, which is the NestJS lesson done properly.

---

## Round 3: the migration seam (J3)

dpg's own design, including how it handles data migrations, is **out of scope here**. You're designing that separately. This section only defines what Galleon expects from *any* migration tool: dpg, goose, golang-migrate, Atlas, or one someone writes themselves.

### The problem the seam has to solve

Migration tools differ in what they can do:

| Ability | Versioned tools (goose, golang-migrate) | Declarative tools (dpg, Atlas) |
|---|---|---|
| Report pending changes | Yes (unapplied files) | Yes (diff from desired state) |
| Plan offline, for CI review | Rarely | Yes |
| Classify risk | No | Often |
| Detect drift | No | Yes |
| Long-running background work | No | Depends on the tool |

A single fat interface would force every tool to fake abilities it doesn't have. So the seam uses the standard Go pattern: **a small required core, plus optional capability interfaces** that Galleon discovers with a type assertion (the way `http.Flusher` and `io.WriterTo` work).

### Required core

```go
package migrate // in Galleon

type Migrator interface {
    // Name identifies this migrator in logs, the CLI and status output.
    Name() string
    // Status compares the database with what this build expects.
    Status(ctx context.Context) (Status, error)
    // Apply brings the database to what this build expects.
    Apply(ctx context.Context, opts ApplyOptions) (Result, error)
}

type State int // UpToDate, Pending, Drifted, Ahead (the database is newer than this build)

type Status struct {
    State    State
    Expected string // version or fingerprint this build expects
    Current  string // version or fingerprint recorded in the database
    Detail   string
}

type ApplyOptions struct {
    Lock     Locker   // Galleon passes its cluster lock; tools must not run without one
    MaxRisk  Risk     // refuse anything riskier (enforced only if the tool can classify)
    Observer Observer // step events, mapped by Galleon to slog and OpenTelemetry
}
```

### Optional capabilities

```go
// Planner: the tool can say what Apply would do, without doing it.
type Planner interface {
    Plan(ctx context.Context) (Plan, error)
}

type Plan interface {
    Steps() []StepInfo
    MaxRisk() Risk
    Fingerprint() string // lets CI approve exactly this plan and deploys check they apply it
}

type StepInfo struct {
    ID, Description string
    Risk            Risk // Safe, Caution, Destructive, Manual, Unknown
}

// OfflinePlanner: the tool can plan without a database connection (CI).
type OfflinePlanner interface {
    PlanOffline(ctx context.Context) (Plan, error)
}

// Verifier: the tool can detect drift beyond "which version was applied".
type Verifier interface {
    Verify(ctx context.Context) (Drift, error)
}

// Backgrounder: the tool has work that must continue after Apply returns.
// Galleon runs it as durable jobs; the tool decides what the work is.
type Backgrounder interface {
    BackgroundTasks(ctx context.Context) ([]BackgroundTask, error)
}

type BackgroundTask interface {
    ID() string
    // Step does one bounded unit of work and reports whether the task is finished.
    // Galleon owns scheduling, leases, throttling, retries and resuming on another node.
    Step(ctx context.Context, cursor []byte) (next []byte, done bool, err error)
}

// Completion: code can ask whether a named task or change is finished.
type Completion interface {
    Done(ctx context.Context, id string) (bool, error)
}
```

**Risk** is a shared scale (Safe, Caution, Destructive, Manual, Unknown). A tool that can't classify reports Unknown, and Galleon's policy decides what Unknown means for a deploy, e.g. "Unknown requires manual approval in production".

### What Galleon does around any migrator

Whatever tool is plugged in, Galleon provides the same behaviour. That's what makes the seam a standard rather than just an interface:

1. **One CLI:** `galleon migrate status | plan | apply`, which uses whichever capabilities the tool has and says clearly when one is missing.
2. **Exactly-once apply across the cluster** through the coordination lock (O3). A migrator never runs without one.
3. **A readiness gate** (A2). At boot, `Status` must be `UpToDate`, or within a policy you set (e.g. tolerate `Ahead` during a rolling deploy). Otherwise the node doesn't take traffic.
4. **A deploy policy:** the maximum risk allowed per environment, plus plan-fingerprint matching when the tool supports `Planner`.
5. **Background tasks** run on Galleon's job system (L1), with progress visible in the operations endpoints (P6).
6. **Several migrators**, for different databases or modules, run in a declared order.
7. **Observability:** every step becomes a span and a structured log.

### Conformance suite

A migrator adapter passes the suite for the core plus each capability it claims. Examples:
- `Apply` twice in a row: the second call changes nothing.
- Two concurrent `Apply` calls under the lock: exactly one runs.
- `Status` is `UpToDate` after a successful `Apply`.
- A plan above `MaxRisk` is refused, not partly applied.
- A background task resumes correctly from a saved cursor after an interruption.

### Where dpg fits

dpg implements the core and whichever capabilities it chooses, through a small adapter module (`galleon-dpg`). Its data migrations, whatever form they take, surface through `Backgrounder` and `Completion` if they need to run past `Apply`. Galleon doesn't need to know anything more about dpg than that.

### Separate from this seam

Data work that isn't tied to schema changes (re-encrypting records, reprocessing) is not a migration. It goes through Galleon's job and data-task seam.

---

## Pushback 1: "A seam for everything" is not the same as "complete"

If Galleon only defines seams, a user starting a project still has to research, choose and wire an adapter for every seam. That's exactly the Chi experience from part 2, with more interfaces. It is the opposite of complete.

**Proposed rule:** *a seam without a default implementation is an incomplete component.* Every seam ships with:

1. **The interface**, small and owned by Galleon.
2. **A default adapter** that works with zero config. For your five areas, the default is your own system. For everything else it's a Galleon battery or a well-chosen third party.
3. **A conformance test suite** (a TCK). This is what makes a seam "standard" in practice and not just an interface. Any adapter, including someone else's, proves it is correct by passing the same suite. Without it, "supports other tools" means "compiles with other tools".
4. **A fake** for tests (T2).

This also answers "correct": the conformance suites are where Galleon's correctness guarantees live.

## Pushback 2: Seams must sit at the touchpoint, not wrap the whole tool

The most common mistake in frameworks with pluggable parts is making the seam a wrapper over an entire category. A generic "ORM interface" is the classic example. You end up with the lowest common denominator of every ORM, ergonomic for none of them.

**Proposed rule:** a seam captures only **what Galleon itself needs to touch**, never the full API of the tool behind it. Users talk to their ORM, IAM or billing system directly, through its own API. Galleon only needs the narrow points where its lifecycle, request pipeline or coordination layer meets that tool.

Applied to your five:

| System | What Galleon actually needs to touch (the seam) | What stays the tool's own API |
|---|---|---|
| **dpg** | Plan, apply under Galleon's cluster lock (exactly once), verify the schema at boot to gate readiness, and enforce a safety policy on plans (J3, O3, V4; revised in Round 2) | The schema DSL, diffing, safety classification |
| **ORM** | A connection pool provider (J1); a **transaction carried in `context`** (J4) that other Galleon parts can join, with typed read and write handles (see Pushback 9); health checks; tracing hooks | Queries, models, relations |
| **comms** | Writing to the outbox in the same transaction as business data (J9); receiving inbound messages as events (L4); dev previews and test fakes (Q1, T2) | Templates, providers, routing, preferences |
| **vantage** | Verifying identity (K2, K3); asking "may X do Y on Z in this context?" (K7); **filtering lists** to what X may see; collecting context for CBAC | Policy authoring, user lifecycle, admin |
| **billing** | Entitlement checks (feature gating); emitting usage events (S5); receiving subscription events | Plans, invoices, payment providers |

The ORM row is the most important one: **the transaction-in-context seam** is what makes the others work. Jobs (L1), the outbox (J9) and audit logs (K15) all have to commit atomically with the user's data, whatever ORM or query layer wrote it. So Galleon has to own the transaction handle, and your ORM (or sqlc, GORM, ent, bun) joins it.

## Pushback 3: Your services all integrate the same way, so Galleon should own that pattern

Look at comms, vantage and billing together. Each one needs:

- **outbound:** the app emits events reliably (outbox),
- **inbound:** the app receives events from the service, exactly once in effect (inbox/dedup),
- **calls:** a typed client with timeouts, retries and circuit breaking,
- **local state:** a cache of what the service knows (policies, entitlements) so the hot path doesn't make a network call,
- **webhooks or a stream** as the transport, with signature verification.

So Galleon needs one **integration kit** in its core: outbox, inbox, event consumer, resilient typed client, local-cache-with-invalidation (on top of O2), and webhook verification. Comms, vantage and billing then each become thin adapters on that kit. So does Stripe, Auth0, Twilio or anyone else a user prefers.

That's the strongest single idea in this document. It makes "integrate with an external service" a first-class, uniform thing instead of three bespoke seams.

**Standard to adopt:** CloudEvents as the envelope for everything that passes through the outbox and inbox. Then comms, vantage, billing and any third party share one wire format.

## Pushback 4: vantage as a pure service would put a network call on every request

Authorization is checked on almost every request, often several times (route, handler, each row of a list). If every check is a call to a remote vantage service:

- latency goes up on every request,
- **vantage being down takes every app down**,
- list filtering ("show me the documents I may see") becomes impossible to do efficiently.

This is the problem that Zanzibar-style systems, OPA and Cerbos solve. The pattern that works:

- **vantage the service** is the source of truth: identities, policies, relationships, audit.
- **vantage the engine** is an embeddable library that evaluates decisions locally against a synced copy of policies, plus the request context. It runs in-process or as a sidecar.
- Freshness is handled by **consistency tokens** (Zanzibar's "zookies") or versioned policy snapshots pushed over the integration kit.

So the answer to "service or library?" is **both, with different jobs**. **You chose this on the decision card.**

**What this means for CBAC specifically:** Galleon is the party that *knows* the request context: IP, device, time, tenant, route, risk signals, MFA freshness, the resource being touched. So Galleon's authorization seam has to include **context providers**, a pipeline that assembles the context object CBAC evaluates. That's a genuinely new seam no framework has today, and it's where Galleon and vantage could be better together than anything else.

**Standards to adopt at this seam:** OIDC for authentication (so Keycloak, Auth0, Entra and others also work), the OpenID **AuthZEN** authorization API for "may X do Y on Z" (designed for exactly this kind of pluggable decision point), and SCIM for user provisioning.

## Pushback 5: billing is closer to authorization than it looks

"Is this tenant on a plan that includes feature X?" and "may this user do X?" are asked at the same moments with the same inputs. If they are two unrelated seams, every handler checks both.

**Proposal:** entitlements from billing become **one more context provider** for vantage's CBAC, or at least share the same check API in Galleon. Then `can(ctx, "export", report)` already covers "the user's role allows it" and "the plan includes it".

Usage metering (S5) needs the cluster-aware counters from O2, then delivery to billing through the outbox.

## Pushback 6: The dependency direction between Galleon and your systems

If Galleon imports your ORM and your ORM imports Galleon, neither can be used standalone, which breaks your "or any other system" goal.

**Proposed rule:**
- Your systems never import Galleon.
- Galleon's core never imports your systems.
- Each seam's interface lives in Galleon. Each adapter (`galleon-dpg`, `galleon-vantage`…) is a small separate module that imports both.
- The default project template wires your adapters in, so the out-of-the-box experience is still "everything is there".

This is the Symfony lesson from part 2.

## Pushback 7: "Correct, complete, ergonomic" will conflict, so rank them

They will collide constantly. A few examples:

- Typed extractors and compile-time DI (correct) vs a quick `c.Get("user")` (ergonomic).
- Refusing to boot when a route has no auth policy (correct) vs getting a prototype up fast (ergonomic).
- Shipping GraphQL, an admin panel and a durable-workflow engine (complete) vs a small core that a team can maintain (correct, and the Buffalo lesson).

You also didn't mention **efficiency**, which is in the project's goal. I'd add it back explicitly.

My proposed ranking: **correct > ergonomic > efficient > complete in the core**, with completeness delivered through batteries and seams rather than a bigger core. The reasoning: a correctness bug in a framework is copied into every app built on it, and completeness is the easiest of the four to add later.

## Pushback 8: What should never be a seam

Some things must be Galleon's own, because a seam there would split the ecosystem or make correctness unprovable:

- **The handler and middleware model** (E1), on top of `http.Handler`.
- **The error model** (H1, H2).
- **Context propagation** (E3) and the **transaction handle** (J4).
- **The coordination layer interface** (O2). Its backends are pluggable (Postgres, Redis, NATS, etcd), but the interface is Galleon's.
- **Lifecycle and process model** (A1–A5).
- **The operation model** (I1), meaning the runtime description of every API operation. The *authoring* format (Themis, or Go types) is a seam. See Pushback 10.
- **Observability**. This should be OpenTelemetry directly, not a Galleon abstraction over it, because OTel already is the standard seam.

Everything else in part 3 is eligible to be a battery or a seam.

---

## Pushback 9: Read and write transaction mixes

You asked for data-access seams that let a module use a specific or random read transaction instead of the primary write transaction. Here's how others handle it, then what I think Galleon's seam has to look like.

### How the prior art does it

| Framework | Mechanism | Read-your-writes guarantee | Weakness |
|---|---|---|---|
| **Rails** | Automatic role switching middleware. POST, PUT, PATCH and DELETE go to the primary; GET and HEAD go to a replica unless the user wrote recently. `connected_to(role: :reading)` forces a block onto a replica. | **Time-based**: the primary is used for a delay window after a write, 2 seconds by default ([Rails guide](https://guides.rubyonrails.org/v6.0/active_record_multiple_databases.html)) | The delay is a guess about replication lag. It is wrong under load, and it only covers the same user. |
| **Laravel** | `read` and `write` host lists per connection, with **a random read host per request**. | **`sticky`**: once a request writes, its later reads go to the write host ([Laravel docs](https://laravel.com/docs/12.x/database)) | Only covers the same request. The next request, or the same user on another node, can read stale data. |
| **Spring** | `@Transactional(readOnly = true)` plus an `AbstractRoutingDataSource` that routes read-only transactions to replicas. `LazyConnectionDataSourceProxy` delays picking a connection until the first statement, so the read-only flag is known in time ([Spring javadoc](https://docs.spring.io/spring/docs/current/javadoc-api/org/springframework/jdbc/datasource/LazyConnectionDataSourceProxy.html)). | None built in | Routing depends on propagation rules people get wrong. A read-only method called inside a write transaction silently joins the write transaction. |
| **Django** | Database routers (`db_for_read`, `db_for_write`) and explicit `.using("replica")`. | None built in | Every team writes its own router. |
| **Ecto (Phoenix)** | A separate replica repo module, chosen explicitly at the call site. | None built in | Explicit and clear, but entirely manual. |
| **Proxies** (ProxySQL, PgCat, PgDog, cloud reader endpoints) | Route at the connection or query level, outside the app. | Some track LSNs | The app's intent (this read must be fresh) is invisible to the proxy. |

**The best mechanism at the database level:** PostgreSQL can answer "has this replica caught up to my write?" exactly. After a write commits, capture the WAL position (`pg_current_wal_lsn()` / `pg_current_wal_insert_lsn()`). Before reading on a replica, wait until it has replayed that position, or route to a replica that already has. Older versions do this by comparing against `pg_last_wal_replay_lsn()`. PostgreSQL 19 adds a `WAIT FOR LSN` command for it ([write-up](https://clickhouse.com/blog/postgresql-19-wait-for-read-your-writes), [DZone pattern](https://dzone.com/articles/postgresql-read-your-write-wal-replica-routing)). This is **exact** where Rails guesses and Laravel's guarantee stops at one request.

### What Galleon's data-access seam needs

**1. Declare intent, never pick a connection.** A module says what it needs, and Galleon picks the source:

```go
// sketch, not a final API
galleon.Write(ctx, func(ctx context.Context, tx db.WriteTx) error { ... })
galleon.Read(ctx, func(ctx context.Context, tx db.ReadTx) error { ... },
    db.FromReplica(db.Any),          // random / least-lag / nearest
    db.Consistency(db.ReadYourWrites))
```

**2. Read and write handles are different types.** `ReadTx` has no write methods, so writing on a replica transaction **doesn't compile**. At runtime Galleon also sets `SET TRANSACTION READ ONLY` as a backstop. This is where Go's type system beats Rails' runtime `prevent_writes` and Spring's annotations.

**3. A consistency menu**, chosen per read:

| Level | Meaning | Mechanism |
|---|---|---|
| `Primary` | Read from the primary | Direct |
| `ReadYourWrites` | See everything this user or session wrote | Causal token (LSN), wait or route |
| `BoundedStaleness(d)` | At most `d` behind | Replica lag tracking |
| `Any` | Whatever is fastest | Random or least-loaded replica |
| `Snapshot(token)` | The exact same snapshot as another transaction | `pg_export_snapshot()` + `SET TRANSACTION SNAPSHOT` |

**4. Causal tokens that travel.** After a write commits, Galleon puts the LSN in the request context, and optionally in a cookie or response header. Then read-your-writes holds **across requests, across nodes, and for mobile clients** that echo the header back. This is your multi-node requirement applied to data, and it fixes the gaps in both Rails' time window and Laravel's per-request stickiness.

**5. Explicit propagation rules.** What happens when a module asks for a read inside someone else's write transaction? Spring's vocabulary is the right one to borrow:
- **Join** (default): reuse the enclosing transaction. A read inside a write sees the uncommitted writes.
- **Independent**: a separate read transaction, possibly on a replica, which **will not see** the enclosing transaction's uncommitted writes.
- **Nested**: a savepoint.
- **Mandatory / Never**: assert that a transaction is or isn't present.

Silently sending a read to a replica while a write transaction is open is the classic bug: the read can't see the enclosing writes, and it may be stale. Galleon should only do that when the module asks with **Independent**, and should flag it in dev.

**6. Several read transactions at once.** A request can hold one write transaction and several read transactions concurrently (fanning out reads for a dashboard, say). With `Snapshot`, those parallel reads all see one consistent view.

**7. Closures and begin/commit pairs (revised, see Round 2).** Both forms share one implementation. A nested `Begin` creates a savepoint, and only the owner of the root transaction can commit it. Only the closure form can retry automatically on serialization failures and deadlocks (SQLSTATE `40001` and `40P01`).

**8. After-commit hooks.** The outbox flush, cache invalidation (J10) and events run only after commit and are discarded on rollback. This is how comms, vantage and billing get correct delivery.

**9. Topology-aware sources.** Named data sources (primary, replicas, analytics replica, per-module databases, shards), each with health checks and lag tracking. Lagging replicas drop out of rotation, and the lag figures are shared through O2.

**10. Never span databases silently.** A write across two databases in one transaction needs two-phase commit, which Galleon should refuse. Use the outbox or a saga instead.

### What this means for your ORM and dpg

The seam is a **`Querier` the ORM pulls from context** (read or write, typed), plus the source registry. The ORM never chooses a host; Galleon does. dpg applies on the primary under the cluster lock, and its verify step gates readiness, so a node never serves against the wrong schema.

---

## Pushback 10: Themis, the API DSL

Themis defines APIs in a DSL with its own generators and validators. Galleon defines the seams that Themis's Galleon generator targets. Galleon may adopt Themis as the default and still let people use something else.

### First, a tension with part 2

In part 2 I took Goa's lesson to be that the contract should be plain Go, not a DSL. Themis is a DSL, so we should be explicit about the costs and how to avoid them:

- **A second language to learn.** That needs an editor experience: a language server, formatting and good errors.
- **A slow loop between edit, generate and run.** Generation has to be fast and run by the dev server on save (U4).
- **Generated code nobody can read.** The output must be plain, readable Go that people can step through in a debugger.
- **Lock-in.** People must be able to leave Themis without rewriting their handlers.

None of these are reasons not to do it. They are requirements on Themis, and they are why Galleon must not *depend* on Themis.

### The key seam: an operation model

What Themis generates and what a Go-first user writes by hand must end up as **the same thing inside Galleon**: an **operation descriptor**. That's a runtime description of one API operation:

- identity (operation ID, name, version, deprecation),
- transport bindings (HTTP method and path, gRPC/Connect service and method, maybe an event topic),
- input and output types, plus their **decoders, encoders and validators**,
- the errors it can return (H3 catalog),
- security requirements (the authentication scheme, and the CBAC action and resource for vantage),
- per-operation policies (timeout, rate limit, idempotency, caching, body limits),
- streaming mode,
- docs metadata (summary, examples, tags).

Everything else in Galleon hangs off these descriptors instead of off any one DSL: routing, OpenAPI and proto output, the docs UI, route introspection (D6), the authorization hook, client generation, and contract diffing (I7). That gives you one source of truth with more than one way to author it. Themis is the recommended way, and Go types with generics (Huma-style) is the other. This changes Pushback 8: what Galleon must own is the **operation model**, not the authoring format.

### The seams Themis plugs into

| Seam | What Galleon defines | What Themis generates |
|---|---|---|
| **Operation registry** | The descriptor type and `Register(op)` | Descriptors for every operation in the DSL |
| **Decoding / binding** (F1, F2) | A `Decoder[T]` interface per content type and source (path, query, header, body) | Reflection-free decoders, which is a real efficiency win over tag-based binding |
| **Encoding** (G1) | An `Encoder[T]` interface | Precompiled encoders (the Fastify lesson) |
| **Validation** (F5, F6) | See below | Validators |
| **Errors** (H1–H3) | The error model and mapping to HTTP and gRPC status | Typed errors from the DSL's error declarations |
| **Security** (K7) | The authorization hook and context providers (Pushback 4) | Action, resource and scope declarations per operation, wired to vantage |
| **Policies** (E5, E9, E10) | Policy keys that middleware reads off the descriptor | Declared limits and timeouts |
| **Contracts** (I2–I4) | An exporter interface and a docs UI that serves the result | OpenAPI, proto and AsyncAPI documents |
| **Clients** (I5) | Nothing. This is entirely Themis's job | Go, TS, Swift, Kotlin and Dart clients |

### The validation seam specifically

Validation will be used by more than Themis: domain code, ORM changesets and form handling all validate too. So the seam has to be shared across layers, not just an API concern:

- **One result shape:** a list of field errors, each with a path (JSON Pointer), a stable code, parameters and a message key for i18n (N10). It maps into the error model and problem+json automatically.
- **Two phases:**
  - **Shape** validation is pure and synchronous: types, ranges, formats, cross-field rules. Themis generates these.
  - **Semantic** validation needs services: "this email isn't taken", "this SKU exists". It gets a `context` and, typically, a **read transaction** from the data seam above. This is where the two topics meet.
- **Composition:** generated validators and hand-written rules combine, and the order is defined.
- **Same validators on the client:** Themis can generate matching TS or mobile validators, so client and server agree.

### Keeping Galleon and Themis independent

The dependency rule from Pushback 6 applies:
- Galleon publishes a small, **versioned target package** (operation model, decoder, encoder and validator interfaces). It's the equivalent of the `protoc` plugin contract, and it falls under Galleon's compatibility promise (X1).
- Themis's Galleon generator lives in Themis (or in a `themis-galleon` module), not in Galleon.
- **Conformance:** an operation generated by Themis and the same operation written by hand in Go must pass the same conformance suite. That's how "use something else" stays true in practice.

---

## Where "most standard seam" means "adopt the existing standard"

Wherever an industry standard exists, it should *be* the seam. Then "other tools" work without a Galleon-specific adapter:

| Area | Standard |
|---|---|
| HTTP handlers | `net/http` (`http.Handler`) |
| SQL | `database/sql` and `pgx` |
| Observability | OpenTelemetry |
| Logging | `log/slog` |
| Feature flags | OpenFeature |
| Authentication | OAuth 2.1 / OIDC, JWT, WebAuthn |
| Authorization decision API | OpenID AuthZEN |
| User provisioning | SCIM 2.0 |
| Events and outbox envelope | CloudEvents |
| REST contracts | OpenAPI 3.1, JSON Schema |
| RPC contracts | Protobuf, gRPC, Connect |
| Event contracts | AsyncAPI |
| Errors | RFC 9457 problem+json |
| Health checks | Kubernetes probe conventions, gRPC health protocol |
| Rate limit headers | IETF `RateLimit` headers |

---

## Open questions for you

Answered questions are recorded in **Decisions so far** at the top. Still open:

1. **CBAC spec:** whenever it's ready. Galleon's seams stay generic in the meantime (D11).
