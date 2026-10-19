# Galleon

A complete, efficient web framework for Go, for internal services and large full-stack applications.

> Status: early. Phase 1 (the kernel) is in progress. APIs will change.

## Repository layout

This repository is a [Go workspace](https://go.dev/ref/mod#workspaces). Each module has its own `go.mod`, and `go.work` at the root lists them all.

| Module | Path | Contents |
|---|---|---|
| `github.com/thec1oud/galleon` | `/` | Kernel (app, modules, DI, config, lifecycle, readiness), the error model, coordination interfaces and the in-process `local` backend |

Planned modules (see [the architecture](docs/design/05-architecture.md#2-module-layout-and-dependency-rules)): `testkit`, `pg`, `coord-pg`, `jobs`, `web`, `realtime`, `http3`, `adapters/*`, `cmd/galleon`.

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
