# Galleon

A complete, efficient web framework for Go, for internal services and large full-stack applications.

Galleon gives you modules, dependency injection and configuration, typed operations over HTTP and RPC, and a PostgreSQL data plane with replicas, read-your-writes and migrations. Every app's wiring, config and coordination are checked at boot, before it takes traffic.

> **Status: early.** APIs will change and there is no release yet. Coordination has only the in-process backend, for a single node. The [Concepts](site/app/docs/concepts/page.mdx) pages say what isn't built yet.

## A first app

```go file=examples/firstapp/main.go#app
func options() []galleon.Option {
	return []galleon.Option{
		galleon.Name("hello"),
		galleon.Coordination(local.New()), // required: there is no default backend
		galleon.Deploy(galleon.SingleNode),
		galleon.Modules(Greetings{}),
	}
}

func main() {
	app, err := galleon.New(options()...)
	if err != nil {
		log.Fatal(err) // every wiring, config and coordination problem, listed together
	}
	if err := app.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
```

A module registers everything it contributes in one place:

```go file=examples/firstapp/greetings.go#module
// Greetings is a module: it registers everything it contributes in one place.
type Greetings struct{}

func (Greetings) Name() string { return "greetings" }

func (Greetings) Register(r *galleon.Registry) error {
	galleon.Config[GreeterConfig](r, "greeter") // typed config, also injectable as *GreeterConfig
	galleon.Provide[*Greeter](r, NewGreeter)    // built at boot from its parameters
	return nil
}

// GreeterConfig is the "greeter" section of the config.
type GreeterConfig struct {
	Greeting string `json:"greeting"`
}

// Default fills in what the config file leaves out.
func (c *GreeterConfig) Default() { c.Greeting = "Hello" }

// Greeter is a service other modules can depend on.
type Greeter struct{ greeting string }

func NewGreeter(cfg *GreeterConfig) *Greeter { return &Greeter{greeting: cfg.Greeting} }

func (g *Greeter) Greet(name string) string { return fmt.Sprintf("%s, %s!", g.greeting, name) }
```

## Documentation

The docs are in [`site`](site), and every code sample in them is tested in CI.

- [Learn](site/app/docs/learn/page.mdx): build a service from zero, one chapter at a time.
- [Guides](site/app/docs/guides/page.mdx): get a specific job done.
- [Concepts](site/app/docs/concepts/page.mdx): how each part works, and why.
- [Reference](site/app/docs/reference/page.mdx): every config key and error code, generated from the source.
- [Decisions](docs/adr/README.md): the architecture decision records, append-only.

To read them locally, with [Bun](https://bun.sh/) installed: `cd site && bun install && bun run dev`, then open http://localhost:3000/docs.

## Contributing

The repository is a Go workspace of several modules. Run every module's checks with:

```sh
bash -ec 'for dir in $(go list -m -f "{{.Dir}}"); do (cd "$dir" && go vet ./... && go test -race ./...); done'
```

Tests that need PostgreSQL, or a streaming replica, skip unless `GALLEON_TEST_POSTGRES` and `GALLEON_TEST_POSTGRES_REPLICA` are set; [.github/scripts/postgres-replica.sh](.github/scripts/postgres-replica.sh) starts both in Docker. [Contributing](site/app/docs/contributing/page.mdx) covers the repository layout, testing and writing the docs.
