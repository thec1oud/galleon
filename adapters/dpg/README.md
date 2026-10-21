# adapters/dpg

`github.com/thec1oud/galleon/adapters/dpg` is the migration seam's adapter for [DPG](https://github.com/thec1oud/dpg), the declarative PostgreSQL schema tool. It is a `migrate.Migrator` that applies the migrations a DPG project's migration log records to one of its databases, through DPG's own apply path.

```go
galleon.Migrator(r, "schema", func(m *pg.Manager) (migrate.Migrator, error) {
    return dpg.New("schema", "db", m.Pool(""))
    // or, in a project with several databases:
    // dpg.New("schema", "db", m.Pool(""), dpg.Database("main", "orders"))
})
```

> **Provisional.** The adapter runs on DPG's library engine (`github.com/thec1oud/dpg/pkg/dpg/engine`), a stopgap API that DPG hasn't published and says will change. Until it is published, the adapter builds only against a local dpg checkout (below), and CI doesn't build it.

## What it does

- **The project's log, and nothing else.** `New` opens the project (the directory a developer runs `dpg` in) and refuses one whose source has changes the log doesn't record: record them with `dpg plan --record`, and check in CI with `dpg plan --check`. The adapter never writes to the project.
- **One migration of the log is one step.** Its risk is its riskiest statement's: DPG's Safe, Caution and Destructive are the seam's; a statement DPG leaves for a person to do is Manual; anything else is Unknown, never Safe. Descriptions name the migration and count its statements, and never hold SQL.
- **Plans and fingerprints.** `Plan` lists what the database hasn't run (and a migration whose statements after its transaction are still to be finished). The fingerprint covers each migration's ID and every statement's stage, safety and SQL, so a plan pinned with `approve` is exactly what runs. `PlanOffline` plans the whole log without a database; its fingerprint isn't the online plan's.
- **Policy before anything runs.** `Apply` refuses, before it creates anything, a database that is `Drifted` or `Ahead` (`dpg.not_pending`), a plan other than the approved fingerprint (`migrate.fingerprint_mismatch`), and a migration above `MaxRisk` or of Unknown risk without `AllowUnknown` (`migrate.risk_refused`); an unset `MaxRisk` means Safe, so a Destructive migration is refused. DPG's own safety gate is set to what the options allow, and DPG's preflight refuses a database whose objects aren't as the migrations expect.
- **Status.** `UpToDate`, `Pending`, `Drifted` and `Ahead` come from DPG's ledger: `Drifted` when the database ran a migration the log doesn't have (another branch, or an edited log), `Ahead` when it ran all of the log and more (a newer build's). `Status` doesn't compare objects; `Verify` does, as `dpg verify` would, reporting the findings `dpg verify` fails for.
- **Fencing.** The hook DPG's engine runs first in each of a migration's transactions raises the migrator's row of a `pg.Fence` (`dpg.galleon_fence`, in DPG's ledger schema, which DPG ignores) to the lease's epoch and token, so the fence commits with the migration, and a node that paused past its lease can't apply anything once it resumes (`dpg.stale_token`, `dpg.stale_epoch`, with the same epoch rules as `pg/sqlmigrate`).
- **Connections.** Each call opens a connection of its own and closes it: a migration may `SET ROLE`, which must not reach the app's connections. It is made as the pool makes its own, through the pool's `BeforeConnect` hook (credential rotation, say) and `AfterConnect` hook, but without the pool's tracer, which may record statements' text: DPG resolves secrets into some statements (user mappings, subscriptions). `BeforeAcquire` and `AfterRelease` don't apply, since the connection is never the pool's.
- **No SQL in errors.** An error names a statement of the log by its step ID (`statement <migration>/<n>`), and leaves out any other statement DPG quotes, such as its ledger records. The error still wraps DPG's, for `errors.Is` and `errors.As`, and keeps what DPG says around it.
- **Instructions.** A statement DPG leaves for a person to do makes its migration Manual, and its text is in the step's description.

It is a `Planner`, `OfflinePlanner` and `Verifier`, and passes `testkit/conformance/migratetest` on Postgres, claiming Ahead, Drift and fencing. It isn't a `Backgrounder`: DPG has no work that continues after a migration, so that case of the suite skips.

### Statements outside a transaction

DPG runs a few statements outside a migration's transaction: a new enum value before it (`ALTER TYPE ... ADD VALUE IF NOT EXISTS`), and after it, statements PostgreSQL refuses in one, chiefly `CREATE INDEX CONCURRENTLY`. Each is checked against the fence in a transaction of its own just before it runs, as is each of DPG's records around it. A holder that pauses between that check and the statement can still run it after its lease has passed to another node, and an apply cancelled while one runs (a lost lease, a shutdown) leaves the migration's transaction committed, with the statements after it to run again: the migration is `Pending`, and the next apply, on any node, finishes it.

Running them again is safe. Enum values and grants repeat harmlessly. Before running a `CREATE INDEX CONCURRENTLY` again, DPG drops an index of that name that a cancelled or failed build left behind invalid (`DROP INDEX CONCURRENTLY IF EXISTS`, fenced like the build), and takes a valid one as built. Two statements DPG may run after a transaction aren't safe to run again, and a person finishes them: `CREATE SUBSCRIPTION`, and `ALTER TABLE ... DETACH PARTITION ... CONCURRENTLY`, which a cancel leaves pending (`... DETACH PARTITION ... FINALIZE`).

## Requirements

- **Role.** The database's owner is enough; no superuser is needed (the tests run the adapter as an owner with no other privilege). The role reads the catalogs DPG introspects, creates DPG's ledger schema (`dpg`, with the fence in it) on the first apply, and needs whatever the migrations themselves do: a migration that creates an object for another owner `SET ROLE`s to it, so the role must be a member of that role, which DPG checks before running anything.
- **cgo.** DPG parses SQL with its fork of `pg_query_go`, a cgo package, so building the adapter needs a C compiler for the target. `GOOS=windows go vet` from Linux doesn't work without a Windows C toolchain; the rest of Galleon vets for Windows without one.

## Building it

The module isn't in the repository's `go.work`, so the workspace builds without dpg. Its `go.mod` replaces `github.com/thec1oud/dpg` with `../../.dpg/core`: link a dpg checkout that has the library engine there, once.

```sh
adapters/dpg/scripts/link-dpg.sh ../dpg   # makes .dpg, which .gitignore ignores
cd adapters/dpg
GOWORK=off go vet ./...
GOWORK=off go test -race ./...            # GALLEON_TEST_POSTGRES as for the rest
```

`GOWORK=off` is needed because the module isn't in the workspace; it then builds against the core and testkit in this repository through its own `replace` directives. To change the core and the adapter together with the workspace's tooling instead, point `GOWORK` at a workspace file of your own, outside the repository, that uses the repository's root, `testkit` and `adapters/dpg` (`GOWORK=$HOME/galleon-dpg.work go test ./...`); don't add the module to the repository's `go.work`.

Its `go.mod` repeats dpg's `replace` of `pg_query_go` (Go takes replaces only from the main module); the link script checks that the two still match.

The tests need a whole database each, since DPG compares a database with its project: they create one on `GALLEON_TEST_POSTGRES` and drop it afterwards. The conformance builds in `testdata/builds` are DPG projects written by `testdata/gen.sh`, which records their logs with the dpg CLI; regenerate them after changing the builds or upgrading DPG.

## Why CI doesn't build it yet

CI vets and tests every module of the workspace, and this one isn't in it, on purpose: it needs dpg's library engine, which isn't published, so a fresh checkout of Galleon can't build it, and CI would have to fetch an unpublished dpg branch. Once DPG publishes the engine (or its successor), the adapter requires a tagged dpg, drops the `.dpg` replace and the link script, joins `go.work`, and CI builds and tests it with the rest.
