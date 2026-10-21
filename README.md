# Galleon

A complete, efficient web framework for Go, for internal services and large full-stack applications.

> Status: early. Phase 1 (the kernel) is in progress. APIs will change.

## Repository layout

This repository is a [Go workspace](https://go.dev/ref/mod#workspaces). Each module has its own `go.mod`, and `go.work` at the root lists them all.

| Module | Path | Contents |
|---|---|---|
| `github.com/thec1oud/galleon` | `/` | Kernel (app, modules, DI, config, lifecycle, readiness, node identity, OpenTelemetry and slog), the clock seam, roles and the process supervisor, the error model, coordination interfaces and the in-process `local` backend, the operation model and HTTP/RPC transport, the outbound client, the data plane (`data` interfaces, `pg` on pgx), and the migration seam (`migrate`, with the built-in `pg/sqlmigrate`) |
| `github.com/thec1oud/galleon/testkit` | `/testkit` | The test harness (`testkit.New` boots the real app on a fake clock and in-process coordination) and conformance suites (`conformance/coordtest`, `conformance/datatest`, `conformance/migratetest`, `conformance/ormtest`), a fake migrator (`testkit.MigrationStore`), `testkit.DB`, a Postgres database per package with a transaction per test, `testkit.Replica`, its view on a streaming replica, and `testkit.Proxy` |

Planned modules (see [the architecture](docs/design/05-architecture.md#2-module-layout-and-dependency-rules)): `coord-pg`, `jobs`, `web`, `realtime`, `http3`, `adapters/*`, `cmd/galleon`.

Dependency rules: the core imports no battery and no adapter; a battery imports only the core's public API; an adapter imports the core plus its one tool.

## A first app

```go
app, err := galleon.New(
    galleon.Name("orders"),
    galleon.Coordination(local.New()), // required: there is no default backend
    galleon.Deploy(galleon.SingleNode),
    galleon.ConfigFile("config.json"),
    galleon.Modules(orders.Module()),
)
if err != nil {
    log.Fatal(err) // every wiring, config and coordination problem, listed together
}
log.Fatal(app.Run(context.Background()))
```

A module registers everything it contributes in one place:

```go
func (Module) Register(r *galleon.Registry) error {
    galleon.Config[DBConfig](r, "db")                 // typed config, also injectable as *DBConfig
    galleon.Provide[*Repo](r, NewRepo)                // func(*DBConfig) (*Repo, error)
    galleon.Provide[*Service](r, NewService)          // func(*Repo, *slog.Logger) *Service
    r.Needs(coord.LeaderElection)                     // checked against the backend at boot
    r.Gate("schema", checkSchema)                     // readiness gate
    return nil
}
```

## Design documents

The design was worked out in [docs/design](docs/design):

1. [Crash course](docs/design/01-crash-course.md)
2. [Strengths and shortcomings](docs/design/02-strengths-and-shortcomings.md)
3. [Complete component list](docs/design/03-complete-component-list.md)
4. [Desires and decisions](docs/design/04-desires-debate.md)
5. [Architecture](docs/design/05-architecture.md)
6. [Seam specifications](docs/design/06-seam-specifications.md)

## Development

```sh
for dir in $(go list -m -f '{{.Dir}}'); do (cd "$dir" && go vet ./... && go test -race ./...); done
```

Tests that need Postgres skip unless `GALLEON_TEST_POSTGRES` holds the DSN of a server they may create databases on, such as `postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable`. Tests that need a streaming replica also skip unless `GALLEON_TEST_POSTGRES_REPLICA` holds the DSN of a standby of that server, whose user may pause and resume replay (a superuser will do). [.github/scripts/postgres-replica.sh](.github/scripts/postgres-replica.sh) starts both in Docker, on ports 5432 and 5433; CI runs it and sets both. With `CI` set, the tests fail instead of skipping.

## Data plane status

Package `pg` routes reads to streaming replicas by read consistency (`Primary`, `Any`, `BoundedStaleness(d)`, `ReadYourWrites` with wait and fail policies), tracks each replica's lag in the background, and carries read-your-writes across requests and nodes with signed causal tokens in the `Galleon-Consistency` header (see [§7.1, §7.4 and §7.5](docs/design/05-architecture.md#71-sources-and-topology)). Not built yet: `Snapshot` reads, sharing lag readings through coordination, replicas of named sources, the token cookie, and `WAIT FOR LSN` on PostgreSQL 19.

## Using an ORM or query builder

Galleon owns the transaction; your ORM, query builder or repository joins it and never opens a connection or chooses a host of its own. Take the handle you are given, or the one in ctx, and fire side effects after the commit:

```go
func (r *Orders) Save(ctx context.Context, o Order) error {
    ctx = data.ForModel(ctx, "order") // spans say galleon.data.model=order, db.collection.name="shop"."orders"
    q, ok := data.QuerierFrom(ctx)    // only inside a write transaction; never a pool
    if !ok {
        return errors.New("orders: Save needs a write transaction")
    }
    if _, err := q.Exec(ctx, "INSERT INTO shop.orders (id, total) VALUES ($1, $2)", o.ID, o.Total); err != nil {
        return err
    }
    return data.AfterCommit(ctx, func(ctx context.Context) { r.events.Placed(ctx, o) }) // never on rollback
}
```

The adapter's module describes its models, and the data module builds the registry (`*data.Models`), failing boot on an invalid model or a duplicate name or table:

```go
galleon.Collect(r, data.ModelInfo{Name: "order", Schema: "shop", Table: "orders", Key: []string{"id"}})
```

`testkit/conformance/ormtest` checks an adapter against the seam's rules, with `ormtest.Reference` as a worked example. See [§7.6](docs/design/05-architecture.md#76-the-orm-seam) and [§6.2](docs/design/06-seam-specifications.md#62-orm-integration). Not built yet: `FilterApplier`, which waits for the security plane, and any named ORM adapter.

## Migrations status

Package `migrate` is the tool-neutral migration seam: a required `Migrator` core plus optional `Planner`, `OfflinePlanner`, `Verifier`, `Backgrounder` and `Completion`. A module registers a migrator with `galleon.Migrator`, and the kernel applies each one exactly once across the cluster under the coordination lock (with its fencing token; `pg/sqlmigrate` also refuses a lease of a superseded coordination epoch), in declared order, gates readiness on its status (`migrate.<name>`), and enforces the deploy policy: the highest risk allowed, what Unknown risk means, and approved plan fingerprints. Boot modes are `off`, `verify` (the Production profile's default) and `apply` (the Development and Test profiles'), set with `galleon.Environment` (in code only, for now) and the `migrate` config section. `pg/sqlmigrate` is the built-in migrator: numbered SQL files from an `fs.FS`. See [§7.7](docs/design/05-architecture.md#77-migrations-j3-d12) and [§6.3](docs/design/06-seam-specifications.md#63-migrations). Not built yet: running background tasks (they need the job system), and the `app migrate status|plan|apply` command, for which `App.Migrations()` is the Go API meanwhile.
