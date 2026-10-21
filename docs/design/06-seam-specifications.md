# Galleon, Part 6: Seam Specifications

Builds on [05-architecture.md](05-architecture.md). Part 5 said *what* the seams are. This part says **exactly what each one promises**, so any implementation (yours, a third party's, a fake) can be checked against it. It's the "most standard seam" goal from part 4, made concrete.

Each seam has the same four sections:
- **Contract:** the Go interface (sketch-level names, real semantics).
- **Rules:** numbered invariants. **MUST** is a requirement that the conformance suite checks. **SHOULD** is strongly recommended and not checked.
- **Conformance:** the tests every implementation has to pass.
- **Ships with:** the default implementation and the test fake (P1).

Module path: `github.com/thec1oud/galleon`.

---

## 0. Open items from part 5, settled with defaults

You can overturn any of these. I've picked the default so work isn't blocked:

| # | Item | Default chosen | Why |
|---|---|---|---|
| 1 | CBAC | The context seam stays generic (D11); this spec defines it (§7.2) | We revisit when vantage's CBAC spec is stable |
| 2 | DI | **Reflection at boot is the default**, with codegen optional | Boot-only cost is within the efficiency budget (D5), and codegen adds a build step for everyone |
| 3 | Stage order | **Authorization is split in two.** An action-level check runs *before* decode (new stage 5b); the resource-level check stays at stage 8 | Callers without permission for the action get `403` before they can learn the input shape from validation errors. The resource check still has the decoded input it needs. |
| 4 | `local` escape hatch | **Kept**, but it requires the explicit setting and logs every affected guarantee at boot, and again in the ops endpoint | Consistent with "explicit, not defaulted" (D7) |
| 5 | Names | `Galleon-Consistency` for the token header; the module split from part 5 §2 | Placeholders until the first release |

---

## 1. Shared conventions (every seam)

1. **MUST take `context.Context` first** on every method that can block, do I/O, or be cancelled. MUST honour cancellation and deadlines.
2. **MUST be safe for concurrent use** unless the type's docs say otherwise (e.g. a transaction handle).
3. **MUST return Galleon errors** (`errs.Error`, §3) for conditions callers branch on. Other errors are wrapped as `Internal`.
4. **MUST NOT use global state.** Everything is constructed and passed in.
5. **MUST NOT log secrets or payload bodies.** Logging goes through the `*slog.Logger` handed in.
6. **MUST create OpenTelemetry spans** for I/O, using the semantic conventions where one exists (HTTP, RPC, DB, messaging).
7. **MUST declare capabilities** where the seam has optional parts, through optional interfaces discovered with a type assertion, never through flags that lie.
8. **Lifecycle:** an implementation that holds resources implements `Start(ctx) error` and/or `Stop(ctx) error`, and the kernel calls them in dependency order.

---

## 2. Kernel

### 2.1 Module and registry

```go
type Module interface {
    Name() string               // unique per app, [a-z0-9_.-]+
    Register(r *Registry) error
}

// Registry: the only way a module contributes anything.
func Provide[T any](r *Registry, ctor any, opts ...ProvideOption)  // ctor: func(deps...) (T[, error])
func Config[T any](r *Registry, section string) *T                 // typed config section
func (r *Registry) Operation(d op.Descriptor, h any)
func (r *Registry) Intercept(at op.Position, i op.Interceptor)
func (r *Registry) Middleware(mw func(http.Handler) http.Handler)
func (r *Registry) Needs(caps ...coord.Capability)
func (r *Registry) Gate(name string, check func(ctx context.Context) error)  // readiness gate
func (r *Registry) Requires(modules ...string)
```

**Rules**
1. `Register` MUST be pure registration: no I/O, no goroutines, no network.
2. A module MUST NOT reach another module's providers except through declared dependencies (`Requires` plus typed injection).
3. Every registration MUST be attributable: errors name the module and the registering call site.
4. Registration order MUST NOT change behaviour except where ordering is explicit (middleware, interceptors at the same position).

### 2.2 Lifecycle

```go
type Starter interface{ Start(ctx context.Context) error }
type Stopper interface{ Stop(ctx context.Context) error }
```

**Rules**
1. Start runs in dependency order and Stop in reverse. Each call has a deadline (default 30s, configurable).
2. A Start failure MUST stop everything already started, in reverse order, and exit non-zero.
3. Ready MUST be reported only when every gate passes. A gate that later fails MUST flip readiness off. Liveness is unaffected.
4. Drain order is fixed: readiness off → grace period → stop listeners → signal long-lived connections → wait for in-flight work → release leases → Stop hooks → flush telemetry.

**Conformance:** order of start and stop; rollback when a start fails; gate flapping; drain order under load; deadline enforcement.

**Ships with:** the kernel itself; `testkit.App` for in-process boot.

---

## 3. Errors

```go
package errs

type Kind uint8 // OK, InvalidArgument, Unauthenticated, PermissionDenied, NotFound,
                // Conflict, FailedPrecondition, TooManyRequests, Canceled, DeadlineExceeded,
                // Unavailable, Unimplemented, Internal

type Error struct {
    Kind    Kind
    Code    string   // stable catalog code: "<module>.<name>"
    Message string   // safe for clients
    Details []Detail // FieldViolations, RetryInfo, QuotaInfo, Help, …
    Cause   error    // never serialized
}
```

| Kind | HTTP | gRPC |
|---|---|---|
| InvalidArgument | 400 | INVALID_ARGUMENT |
| Unauthenticated | 401 | UNAUTHENTICATED |
| PermissionDenied | 403 | PERMISSION_DENIED |
| NotFound | 404 | NOT_FOUND |
| Conflict | 409 | ALREADY_EXISTS / ABORTED |
| FailedPrecondition | 412 / 422 | FAILED_PRECONDITION |
| TooManyRequests | 429 | RESOURCE_EXHAUSTED |
| Canceled | 499 | CANCELLED |
| DeadlineExceeded | 504 | DEADLINE_EXCEEDED |
| Unavailable | 503 | UNAVAILABLE |
| Unimplemented | 501 | UNIMPLEMENTED |
| Internal | 500 | INTERNAL |

**Rules**
1. The HTTP body MUST be RFC 9457 problem+json: `type` derived from `Code`, `title` from `Message`, `status`, `instance` = trace ID, and extensions for details.
2. `Cause` and internal messages MUST NEVER be sent to clients. In dev, they appear only on the debug page.
3. `Code` values MUST be stable once released. Removing or renaming one is a breaking change that contract diffing catches (I7).
4. Every non-`errs.Error` error that reaches the edge MUST become `Internal`, logged with its full cause and the trace ID.

---

## 4. Coordination

```go
package coord

type Backend interface {
    Capabilities() Capabilities
    Membership() Membership
    Locks() Locker
    Leader(name string) Election
    PubSub() PubSub
    KV() KV
    Counters() Counters
}

type Locker interface {
    // Acquire blocks until the lease is held or ctx ends.
    Acquire(ctx context.Context, key string, ttl time.Duration) (Lease, error)
    TryAcquire(ctx context.Context, key string, ttl time.Duration) (Lease, bool, error)
}

type Lease interface {
    Token() uint64                     // fencing token, strictly increasing per key within Epoch
    Epoch() string                     // the token counter's identity: tokens compare only within one
    Renew(ctx context.Context) error
    Release(ctx context.Context) error
    Lost() <-chan struct{}             // closed if the lease is lost (expiry, partition)
}

type Election interface {
    Campaign(ctx context.Context) (Lease, error) // returns once this node is leader
    Leader(ctx context.Context) (NodeID, bool, error)
    Observe(ctx context.Context) <-chan NodeID
}

type PubSub interface {
    Publish(ctx context.Context, topic string, msg []byte) error
    Subscribe(ctx context.Context, topic string) (Subscription, error)
}

type KV interface {
    Get(ctx context.Context, key string) (Entry, error)            // Entry{Value, Revision}
    Put(ctx context.Context, key string, v []byte, o ...PutOpt) (uint64, error) // TTL, IfRevision
    Delete(ctx context.Context, key string, o ...DeleteOpt) error
    Watch(ctx context.Context, prefix string) (<-chan Event, error)
}

type Counters interface {
    Add(ctx context.Context, key string, delta int64, window time.Duration) (int64, error)
}
```

**Rules**
1. **Mutual exclusion:** at most one holder of a key's lease at a time, *as judged by fencing token*. A protected resource that checks tokens MUST reject a lower token than one it has seen.
2. Fencing tokens MUST strictly increase per key within an epoch, and a backend MUST keep its epoch across restarts unless its tokens restart too, in which case it MUST report a new one (`local` takes a random epoch per instance). A protected resource compares tokens only within an epoch; it lets a lease of an epoch it hasn't seen take over, records the epoch that was replaced, and refuses every later lease of a replaced epoch, whatever its token.
3. `Lost()` MUST close before another node can acquire the same key, or the lease's TTL MUST be shorter than the window in which a holder can still act. The backend documents which guarantee it gives. A lease's TTL MUST NOT start before the `TryAcquire` or `Renew` call that grants or extends it begins, and an `Acquire`'s MUST NOT start before the lease is granted, however long the call waited, so `Acquire` never returns an expired lease. A holder times its deadline from before the call; since it can't tell when during a long `Acquire` the grant happened, it renews at once and times from that `Renew` (the migration Runner does), and asks again if the backend says the lease was lost.
4. PubSub is **at-most-once**, with no ordering guarantee across publishers. Anything that needs at-least-once uses the outbox (§8), not PubSub.
5. KV `Put` with `IfRevision` MUST be atomic compare-and-swap.
6. `Capabilities()` MUST be honest: `local` reports `Scope: Process`; the others report `Scope: Cluster`.
7. Membership MUST detect a failed node within a configured bound, and that bound is reported in `Capabilities()`.

**Conformance:** contention between many acquirers; fencing monotonicity; lease expiry during a pause (process frozen, then resumed); the leader failover bound; CAS races; watch delivery after reconnect; partition tests through the cluster harness.

**Ships with:** `local` (in core) and `coordpg` as the first cluster backend; `testkit.Coord` (local, plus fault injection: pause, partition, clock skew).

---

## 5. Operations, pipeline and codecs

### 5.1 Descriptor and invocation

```go
package op

type Invoker func(ctx context.Context, call *Call) (any, error)
type Interceptor func(next Invoker) Invoker

type Call struct {
    Desc    *Descriptor
    Input   any            // set after Decode
    Request *Request       // transport-neutral view: headers/metadata, peer, binding
    Response *Response     // headers/metadata interceptors add to the answer; nil for in-process calls
}

type Position struct{ Stage Stage; Where Where } // Before or After a stage

const (
    StageRoute Stage = iota + 1
    StageAdmit
    StageAuthenticate
    StageResolveContext
    StageGate
    StagePreAuthorize    // 5b: action-level, before decode
    StageDecode
    StageValidateShape
    StageAuthorize       // resource-level
    StageValidateMeaning
    StageHandle
    StageCommitHooks
    StageEncode
)
```

**Rules**
1. Descriptor IDs MUST be unique per app and stable across releases.
2. A descriptor MUST NOT be registered without a `Security` declaration or an explicit `Public()`. Boot fails otherwise.
3. Stages MUST run in the fixed order for every transport (HTTP, RPC, events, jobs). Transport-specific stages are skipped, never reordered.
4. An interceptor MUST call `next` at most once, MUST NOT call it after returning, and MUST propagate the `ctx` it received (it may derive from it).
5. Decoders, encoders and the interceptor chain MUST be prepared at boot. Per request, the pipeline MUST NOT use reflection or allocate a new chain.
6. A handler's input and output types MUST match its descriptor's, checked at registration.

### 5.2 Codecs and validation

```go
type Decoder[T any] interface {
    Decode(r *Request, into *T) error // errs.InvalidArgument with FieldViolations on bad input
}
type Encoder[T any] interface {
    ContentType() string
    Encode(w ResponseWriter, v *T) error
}

type Validator[T any] interface {
    Validate(ctx context.Context, v *T) *ValidationError
}
type MeaningValidator[T any] interface { // stage 9; ctx carries a read transaction
    ValidateMeaning(ctx context.Context, v *T) *ValidationError
}

type ValidationError struct{ Violations []Violation }
type Violation struct {
    Path   string         // JSON Pointer, e.g. "/items/3/quantity"
    Code   string         // stable, e.g. "min", "required", "orders.sku_unknown"
    Params map[string]any // for message rendering
    MsgKey string         // i18n key
}
```

**Rules**
1. Shape validators MUST be pure: no I/O, deterministic, safe to run on the client too.
2. Meaning validators MAY do I/O, but only through the read transaction in `ctx`. They MUST NOT write.
3. Violation `Path` values MUST be valid JSON Pointers into the *wire* representation, not the Go struct.
4. Decoders MUST enforce the operation's body limit while streaming, not after buffering.
5. Unknown fields: the default is **reject**, configurable per operation (Themis can declare it).

**Conformance (for generated or custom codecs):** round trip for every type in a golden corpus; parity between generated validators and reference validators on a shared corpus; body-limit enforcement; violation paths for nested and array fields.

**Ships with:** boot-time-planned codecs and validators in core; Themis generates the reflection-free ones; `testkit` provides corpus runners.

---

## 6. Data

### 6.1 Sources and handles

```go
package data

// CommandTag, Rows, Row, Batch, BatchResults, Identifier and CopyFromSource are pgx's types.
type Reader interface {
    Query(ctx context.Context, sql string, args ...any) (Rows, error)
    QueryRow(ctx context.Context, sql string, args ...any) Row
    SendBatch(ctx context.Context, b *Batch) BatchResults
}

type Querier interface {
    Reader
    Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
    CopyFrom(ctx context.Context, table Identifier, cols []string, src CopyFromSource) (int64, error)
}

type ReadTx interface {
    Reader                                     // no Exec or CopyFrom: a write doesn't compile
    Begin(ctx context.Context) (ReadTx, error) // savepoint
    Commit(ctx context.Context) error
    Rollback(ctx context.Context) error
}

type WriteTx interface {
    ReadTx
    Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
    CopyFrom(ctx context.Context, table Identifier, cols []string, src CopyFromSource) (int64, error)
    BeginWrite(ctx context.Context) (WriteTx, error)
    AfterCommit(fn func(ctx context.Context))
    AfterRollback(fn func(ctx context.Context))
}

type Manager interface {
    BeginRead(ctx context.Context, o ...ReadOpt) (ReadTx, error)
    BeginWrite(ctx context.Context, o ...WriteOpt) (WriteTx, error)
    Read(ctx context.Context, fn func(context.Context, ReadTx) error, o ...ReadOpt) error
    Write(ctx context.Context, fn func(context.Context, WriteTx) error, o ...WriteOpt) error
}

// Options: Propagation (Join, Independent, Nested, Mandatory, Never: each value is an option),
// Consistency(level) [reads only]: Primary, Any, BoundedStaleness(d), ReadYourWrites[.Wait(d)][.OrFail()];
//   Snapshot(tok) is deferred. An operation's default is Descriptor.Consistency; WithConsistency(ctx, l)
//   carries it. The primary's source name is PrimarySource ("primary").
// Source(name), Isolation(level), Retry(policy) [closure form only; Retry(policy).Do(ctx, fn) reruns pair code]

func With(ctx context.Context, tx ReadTx) context.Context // transactions begun with ctx join or nest in tx
func From(ctx context.Context) ReadTx                    // the closure forms set it for fn's ctx
```

**Rules**
1. **Join is the default** (D8). Inside a write transaction, `BeginRead` with `Join` returns a read view of that same transaction.
2. Only the root owner's `Commit` commits. A nested handle's `Commit` MUST only release its savepoint.
3. `Rollback` after `Commit` MUST be a no-op that returns nil. `Commit` after `Rollback` MUST return an error.
4. A read transaction, on a replica or the primary, MUST run `READ ONLY` (`BEGIN … READ ONLY`, or `SET TRANSACTION READ ONLY`), at its requested isolation level.
5. `AfterCommit` callbacks MUST run only after the root transaction commits, in registration order, and MUST NOT run on rollback. A callback registered on a nested handle belongs to the root, and is dropped if that handle's savepoint rolls back. `AfterRollback` callbacks run when the work they were registered with is rolled back: the root's (a failed commit included), or a nested handle's savepoint.
6. Closure form: on SQLSTATE `40001` or `40P01`, the whole closure MUST be retried under the retry policy, when it owns the root transaction; a joined or nested closure MUST NOT retry, and its error goes to the owner. `AfterCommit` registrations from failed attempts MUST be discarded; each failed attempt is a rollback, so its `AfterRollback` callbacks run. The pair form never retries. Serialization failures and deadlocks are `Conflict` errors with a `RetryInfo` detail.
7. `ReadYourWrites` MUST never return data older than the causal floor: the request's own commits, and the causal token it brought. If no replica qualifies within the wait budget, it MUST read from the primary or fail with `Unavailable` (`data.consistency_timeout`), as the policy says. With no replicas, every level MUST read the primary without failing. A read that joins or nests in a transaction MUST stay on it, whatever its level. A level that fails validation, from the call's options or from ctx, MUST fail the read with `Internal` (`data.invalid_consistency`), never fall back to another level; an operation that declares one MUST fail registration where the op plane can tell, and otherwise each of its calls MUST fail the same way.
8. A transaction open at the end of its owning request or job MUST be rolled back and reported (an error in dev and test).
9. Two-phase commit across sources MUST be refused.
10. A call that committed a write MUST send a causal token (`Galleon-Consistency`) when tokens are configured, even when the write's WAL position couldn't be read after its commit. A call that didn't MUST NOT, unless it reissues the token it brought, lowered to the primary's position (a read may do this). A response with a token MUST NOT be storable by a shared cache. A token that fails verification MUST be ignored, never fail the call.

**Conformance:** savepoint semantics; ownership (a nested `Commit` doesn't commit the root); commit hooks on commit, rollback and retry; retry on injected serialization failures; read-only enforcement; every level without replicas, and reads in write transactions staying there; causal tokens sent only after a commit and ignored when bad; read-your-writes, across requests and within one, with its wait and fail policies, against a real streaming replica whose replay the suite pauses; leak detection.

**Ships with:** `galleon/pg` (pgx, a package of the core module); `testkit/conformance/datatest`; `testkit.DB` (template database per package, transaction per test, through `pg.Within`; it skips without `GALLEON_TEST_POSTGRES`, and fails without it in CI); `testkit.Replica` (the package's database on a streaming standby, `GALLEON_TEST_POSTGRES_REPLICA`, with replay to pause and resume, taking turns across packages); `testkit.Proxy`, a TCP proxy that cuts a database off and records what clients send. Pausing replay replaced the lag-injecting proxy: it lags exactly, and only when asked.

### 6.2 ORM integration

An ORM adapter MUST:
1. execute through the `Querier` or transaction it's given, never through its own pool when one is supplied;
2. never choose a host or replica itself;
3. register its hooks via `AfterCommit` rather than firing side effects inline.

An ORM adapter MAY:
- expose model metadata (`ModelInfo`) for tracing and the admin battery;
- implement `FilterApplier`, which turns an authorization `Filter` (§7.3) into a `WHERE` clause.

### 6.3 Migrations

```go
package migrate // no driver dependency

type Migrator interface {
    Name() string                                          // [a-z0-9_.-]+, unique per app
    Status(ctx context.Context) (Status, error)
    Apply(ctx context.Context, opts ApplyOptions) (Result, error)
}

type State uint8   // UpToDate, Pending, Drifted, Ahead
type Status struct{ State State; Expected, Current, Detail string }
type Risk uint8    // Unknown (the zero value), Safe, Caution, Destructive, Manual; ordered Safe < … < Manual < Unknown

type ApplyOptions struct {
    Lock         coord.Lease // held; Token() is the fencing token
    MaxRisk      Risk        // zero (Unknown) means Safe
    AllowUnknown bool        // whether Unknown steps may run
    Fingerprint  string      // the plan checked or approved; "" for a migrator that can't plan
    Observer     Observer    // may be nil: use opts.Step(ctx, s)
}
type Observer interface{ Step(ctx context.Context, s StepInfo) (context.Context, func(err error)) }
type Result struct{ Applied []StepInfo; Detail string }

// Optional capabilities, found by type assertion:
type Planner interface{ Plan(ctx) (Plan, error) }                 // Plan: Steps, MaxRisk, Fingerprint
type OfflinePlanner interface{ PlanOffline(ctx) (Plan, error) }
type Verifier interface{ Verify(ctx) (Drift, error) }             // Drift{Differences []string}
type Backgrounder interface{ BackgroundTasks(ctx) ([]BackgroundTask, error) }
type BackgroundTask interface{ ID() string; Step(ctx, cursor []byte) (next []byte, done bool, err error) }
type Completion interface{ Done(ctx, id string) (bool, error) }

// The kernel side: galleon.Migrator(r, name, ctor, MigrateAfter(…) | MigrateOrder(n)),
// migrate.Runner (Status, Plan, Apply, ApplyOne, Done) and migrate.Policy.
```

Three changes from part 4's sketch: `ApplyOptions.Lock` is the held lease rather than a `Locker`, since the Runner takes the lock before calling any migrator code; `AllowUnknown` sits beside `MaxRisk`, because whether Unknown may run is its own policy, not a point on the scale; and an unset `MaxRisk` means Safe, so options nobody filled in are the most restrictive. `coord.Lease` gained `Epoch` (§4), so a fence can tell a restarted token counter from a stale holder.

**Rules**
1. `Apply` MUST NOT run without `opts.Lock`, and MUST stop when ctx ends. The Runner never calls it without a held lease, and cancels ctx as soon as a renewal reports `coord.ErrLeaseLost`, `Lost()` closes, or no renewal has succeeded by one renewal interval short of the TTL, so ctx ends before the lease can expire.
2. A migrator that classifies risk MUST refuse, before changing anything, changes with a step that `opts` doesn't allow (above `MaxRisk`, which means Safe when unset, or Unknown without `AllowUnknown`). A `Planner` MUST refuse a plan whose fingerprint isn't `opts.Fingerprint`, when that is set.
3. `Apply` on an up-to-date database MUST change nothing, and `Status` MUST be `UpToDate` after an `Apply` that returned without error. The Runner reads `Status` under the lock before applying, and again after.
4. A migrator that claims fencing MUST refuse a lease whose token is below one it has applied under in the same epoch, as recorded with the change itself. It MUST let a lease of an epoch it hasn't seen take over, and from then on MUST refuse every lease of the epoch that was replaced, whatever its token, so a superseded holder can't take the fence back (§4, rule 2).
5. `Status` SHOULD NOT change the database (sqlmigrate's own tests check that it creates nothing).
6. `Ahead` and `Drifted` MUST NOT be applied over: `Apply` refuses them, as does the Runner.
7. A `BackgroundTask`'s `Step` MUST be safe to repeat from the same cursor, since a step interrupted before its cursor is saved runs again.
8. Several migrators MUST run in their declared order, and the first failure MUST stop the rest.
9. Spans, logs and metric attributes MUST NOT hold SQL text or secrets: a step's ID and risk are recorded, its description only with the debug setting.

The kernel's behaviour around any migrator (the lock, the readiness gate, boot modes, the per-environment policy, fingerprint pinning, observability and the operator surface) is described in [part 5, §7.7](05-architecture.md#77-migrations-j3-d12).

**Conformance** (`testkit/conformance/migratetest`): an empty database is `Pending`; `Apply` twice changes nothing the second time; four concurrent nodes under one lock run `Apply` exactly once; `Status` is `UpToDate` after `Apply`; a plan above `MaxRisk` is refused by the Runner, and by the migrator itself when it can plan, with nothing applied; `Apply` without a lease is refused, as is a Destructive step with `MaxRisk` unset; and, where claimed, a stale lease is refused, a new epoch takes over, through chosen leases and through a restarted in-process backend, and the epoch it replaced is refused afterwards (fencing), `Ahead` and `Drifted` are detected and not applied over, plans are stable across nodes and pinned fingerprints are enforced (`Planner`), offline plans don't depend on the database (`OfflinePlanner`), `Verify` finds the edited change (`Verifier`), and a background task resumes on another node from a saved cursor, repeating a step safely, until `Done` (`Backgrounder`, `Completion`).

**Ships with:** `pg/sqlmigrate` (numbered SQL files from an `fs.FS`; `Planner`, `OfflinePlanner` and `Verifier`, fencing, Ahead and Drifted, out-of-order files refused unless allowed), which passes the suite against real Postgres; `testkit.MigrationStore` and `FakeMigrator` (every capability, shared state across nodes, hooks to block or fail a step), and `testkit.CoreOnly` to hide its capabilities, which pass it too. **Deferred:** running background tasks, which needs the job system (§9); the `migrate` CLI (phase 8), for which the Go API stands in.

---

## 7. Security

### 7.1 Identity

```go
package security

type Authenticator interface {
    Scheme() string // "session", "bearer", "apikey", "mtls", …
    Authenticate(ctx context.Context, r *op.Request) (Principal, error)
}

type Principal interface {
    Subject() string
    Kind() PrincipalKind     // User, Service, APIKey, Anonymous
    Issuer() string
    Claims() Claims          // Get(name) (any, bool); typed helpers
    Assurance() Assurance    // AuthTime, Methods (pwd, otp, webauthn…), Level
}
```

**Rules**
1. Return `ErrNoCredentials` when the request carries no credentials for this scheme, so the next scheme is tried. Return `Unauthenticated` when credentials are present but invalid. Never try another scheme after invalid credentials.
2. Token verification MUST check signature, `exp`, `nbf`, `iss` and `aud`, with a bounded clock-skew allowance.
3. Key material (JWKS and the like) MUST be cached and refreshed in the background. Rotation MUST NOT cause a burst of failed requests.
4. Revoked sessions and tokens MUST be rejected cluster-wide within the revocation bound declared in `Capabilities` (via coordination KV).

### 7.2 Operating context (the CBAC slot)

```go
type ContextResolver interface {
    Resolve(ctx context.Context, p Principal, r *op.Request, in Inputs) (OperatingContext, error)
}

type ContextProvider interface {
    Name() string
    Provide(ctx context.Context, p Principal, r *op.Request) (Attributes, error)
}

type OperatingContext interface {
    Domain() string
    ID() string
    Attributes() Attributes
    Proof() Proof            // verifiable without I/O
    ExpiresAt() time.Time
}

type ContextSwitcher interface { // optional capability
    Switch(ctx context.Context, p Principal, target ContextRef) (OperatingContext, error)
    Revoke(ctx context.Context, ref ContextRef) error
}
```

**Rules**
1. **Resolution MUST NOT do network I/O on the hot path** when the request carries a valid proof. Resolvers that need I/O MUST be marked `Slow()`, and Galleon reports them at boot.
2. `Inputs` contains only the providers the operation's descriptor names. Providers are composable and evaluated lazily.
3. The gate (stage 5) MUST be a pure function of the descriptor and the `OperatingContext`.
4. A switch MUST issue a new proof, and the old one MUST be revoked cluster-wide within the declared bound.
5. Without a configured resolver, Galleon uses the identity resolver (the principal's own context), and every operation that declares `Contexts(...)` fails boot. That avoids silently allowing everything.

### 7.3 Decisions

The query shape follows the **OpenID AuthZEN** Authorization API's subject, action, resource and context, so AuthZEN-compliant engines plug in with a thin adapter.

```go
type Decider interface {
    Check(ctx context.Context, q Query) (Decision, error)
    CheckMany(ctx context.Context, qs []Query) ([]Decision, error)
}

type Query struct {
    Subject  Subject           // from Principal
    Action   string            // "orders:cancel"
    Resource Resource          // Type, ID, Attributes (nil at stage 5b)
    Context  OperatingContext
}

type Decision struct {
    Allow   bool
    Reason  string            // for audit and dev, never sent to clients
    TTL     time.Duration     // how long this decision may be reused within the request
    Obligations []Obligation  // e.g. "mask:field", "audit"
}

type Filterer interface { // optional
    Filter(ctx context.Context, q FilterQuery) (Filter, error) // a predicate tree over resource attributes
}
```

**Rules**
1. **Default deny:** an engine error MUST be treated as deny (`Unavailable` to the client), never as allow.
2. `CheckMany` MUST return results in the same order as its input.
3. Obligations the pipeline doesn't understand MUST cause a deny, never be silently ignored.
4. A `Filter` MUST be sound: every row it admits must pass `Check`. The conformance suite verifies this against a sample dataset.
5. Every decision MAY be audited. When `audit` is requested, the event MUST go through the outbox in the same transaction as the action it authorized.

**Conformance:** the rules above; batch ordering; filter soundness; behaviour on engine timeouts; propagation of revocations; switch-then-revoke.

**Ships with:** a policy-table decider (role → actions) in core; an OIDC authenticator; the identity resolver; `testkit.Security` (a scripted principal, context and decision per test).

---

## 8. Integration kit

```go
package integration

// Events use CloudEvents 1.0 attributes.
type Event struct {
    ID, Source, Type, Subject string
    Time        time.Time
    DataSchema  string
    ContentType string
    Data        []byte
    Extensions  map[string]string // traceparent, partitionkey, tenant, …
}

type Outbox interface {
    // Enqueue MUST be called with a WriteTx; the event exists iff tx commits.
    Enqueue(ctx context.Context, tx data.WriteTx, e Event, o ...EnqueueOpt) error
}

type Sink interface { // a destination the relay delivers to
    Name() string
    Deliver(ctx context.Context, batch []Event) ([]Result, error)
}

type Inbox interface {
    // Seen records (source, id) in tx; returns true if already processed.
    Seen(ctx context.Context, tx data.WriteTx, source, id string) (bool, error)
}

type Verifier interface { // inbound webhook signature schemes
    Verify(r *http.Request, body []byte) error
}
```

**Rules**
1. **Atomicity:** an outbox event is visible to the relay if and only if its transaction committed.
2. **Delivery is at-least-once**, ordered per partition key. Different partition keys have no ordering between them.
3. The relay MUST hold a coordination lease per partition and MUST stop delivering as soon as `Lost()` fires.
4. Failed deliveries MUST retry with exponential backoff and jitter. After the configured attempts, an event moves to the dead-letter table and is never silently dropped.
5. **Inbox processing is exactly-once in effect:** `Seen` and the handler's writes commit in the same transaction.
6. Webhook verification MUST check a timestamp to prevent replay, within a bounded window.
7. Trace context MUST be carried in CloudEvents extensions (`traceparent`) across the boundary.

**Conformance:** crash between commit and relay (the event still arrives); duplicate delivery (the effect happens once); ordering per key under concurrency; relay failover; dead-lettering and replay; signature verification and replay rejection.

**Ships with:** a Postgres outbox and inbox (`galleon/pg`), an HTTP webhook sink, standard verifiers (HMAC with timestamp); `testkit.Integration` (captures events, scripts failures).

---

## 9. Jobs and scheduling

```go
package jobs

type Backend interface {
    Enqueue(ctx context.Context, j Job, o ...EnqueueOpt) (ID, error) // joins the WriteTx in ctx if present
    Fetch(ctx context.Context, queues []string, n int) ([]Lease, error)
    Complete(ctx context.Context, l Lease) error
    Fail(ctx context.Context, l Lease, err error, retryAt time.Time) error
    Extend(ctx context.Context, l Lease, d time.Duration) error
}

type Scheduler interface {
    Register(s Schedule) error // runs once per tick per cluster unless EveryNode
}
```

**Rules**
1. Enqueueing inside a write transaction MUST be transactional: the job exists iff the transaction commits.
2. A job's lease MUST carry a fencing token. `Complete` and `Fail` with a stale token MUST be rejected.
3. A job held by a node that dies MUST become available again after its lease expires, and handlers MUST be written to be idempotent (documented and lint-checked).
4. Uniqueness MUST be enforced atomically at enqueue.
5. **Schedules:** exactly one enqueue per tick across the cluster, attributed to the current leader. Missed ticks follow the declared policy (`Skip`, `RunOnce`, `RunAll`).
6. Jobs run through the operation interceptor chain, with the same telemetry, error model and transaction scoping.

**Conformance:** transactional enqueue; lease expiry and recovery; fencing rejection; uniqueness under concurrency; schedule singleton across a three-node cluster with leader failover; missed-tick policies.

**Ships with:** a Postgres backend; `testkit.Jobs` (inline, step-by-step or manual drain).

---

## 10. Cache, storage, flags, secrets, config

### Cache

```go
type Store interface { // L2
    Get(ctx context.Context, key string) ([]byte, bool, error)
    Set(ctx context.Context, key string, v []byte, ttl time.Duration, tags ...string) error
    Delete(ctx context.Context, keys ...string) error
    InvalidateTags(ctx context.Context, tags ...string) error
}
```
**Rules**
1. L1 invalidation MUST propagate cluster-wide through coordination PubSub.
2. Write-triggered invalidation MUST be registered with `AfterCommit`.
3. Loads MUST be deduplicated per key (singleflight).
4. A cache failure MUST degrade to the source of truth, never to an error, unless the cache is configured as required.

### Storage

```go
type Bucket interface {
    Open(ctx context.Context, key string) (io.ReadCloser, Info, error)
    Create(ctx context.Context, key string, o ...WriteOpt) (io.WriteCloser, error) // streaming
    Delete(ctx context.Context, key string) error
    List(ctx context.Context, prefix string) iter.Seq2[Info, error]
    SignedURL(ctx context.Context, key string, op Op, ttl time.Duration) (string, error) // optional capability
}
```
**Rules**
1. Writes MUST be atomic: a reader never sees a partial object.
2. The `local` bucket reports `Scope: Node`, and boot MUST refuse it in a multi-node deployment without an explicit override.

### Feature flags
**The seam is OpenFeature's provider interface**, adopted directly. Galleon adds only:
1. The evaluation context is filled from the principal and `OperatingContext`.
2. Flag changes propagate cluster-wide.

### Secrets and config sources

```go
type SecretSource interface {
    Get(ctx context.Context, name string) (Secret, error)    // Secret redacts itself in fmt and slog
    Watch(ctx context.Context, name string) (<-chan Secret, error) // optional
}
type ConfigSource interface {
    Name() string
    Load(ctx context.Context) (map[string]any, error)
    Watch(ctx context.Context) (<-chan struct{}, error)       // optional
}
```
**Rules**
1. `Secret` values MUST print as `[REDACTED]` through `fmt`, `slog` and JSON encoders.
2. Every config value MUST record its source for introspection.

---

## 11. Web battery

```go
type Renderer interface { // templ is the default implementation
    Render(ctx context.Context, w io.Writer, c Component) error
}
type Assets interface {   // Vite is the default implementation
    URL(entry string) (string, error)        // fingerprinted URL
    Tags(entry string) (template.HTML, error) // <script>/<link> tags incl. preloads
    DevProxy() http.Handler                   // nil in production
}
```
**Rules**
1. Rendering MUST receive the CSP nonce and CSRF token through `ctx`. Components never generate their own.
2. Fragment responses MUST set `Vary` on the fragment-request header so caches don't mix full pages and fragments.
3. In production, `Assets` MUST be served from embedded files, with immutable cache headers on fingerprinted paths.

---

## 12. Conformance suites: how they're delivered

- Each seam has a package `testkit/conformance/<seam>` with a single entry point:

```go
func TestMyBackend(t *testing.T) {
    conformance.Coord(t, func(t *testing.T) coord.Backend { return newBackendForTest(t) })
}
```

- Suites are **versioned with Galleon's core**. An adapter declares which suite version it passes, and the adapter registry shows it.
- Suites that need several nodes run through the cluster harness. Suites that need a real database start one from a template, the same way `testkit.DB` does.
- A **capability you claim is a suite you must pass.** Claiming `Filterer` or `SignedURL` without passing that part of the suite fails the run.

---

## 13. What this spec doesn't settle yet

1. **Event operation details:** how Themis's event declarations map onto `op.Descriptor` with `Kind: Event`. This needs Themis's event model.
2. **The realtime battery contracts** (channels, presence, resume). These come after the coordination backends exist, so the spec can be written against real behaviour.
3. **The exact `Proof` format** for operating contexts. Galleon only requires that it's verifiable without I/O. The format belongs to the IAM (vantage or another).
4. **The adapter registry:** where third-party adapters and their suite versions are listed.
