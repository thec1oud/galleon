# 0500. Run commands as subcommands of the app's own binary

Status: Proposed
Date: 2026-10-22

## Context

The migration seam (0019) needs an operator surface: `migrate status`,
`migrate plan` and `migrate apply`. The design had it both ways: the module
layout listed a separate `cmd/galleon` CLI, while the build plan wrote
`app migrate`, the app's own binary.

Whatever runs the command must have the app's migrators, and they exist
only in the app's code. A module registers each one in Go, with a
constructor that takes the app's providers, such as `*pg.Manager`, and the
policy comes from the app's config, environment profile and
`galleon.MigrationPolicy`. A separate binary can't load any of that by
itself.

## Decision

Commands are subcommands of the app's binary. Package `cli`, in the core
module, runs them: a `main` hands it its arguments with
`os.Exit(cli.Main(ctx, os.Args[1:], options()...))`. Without a command the
app serves; `migrate status|plan|apply` act on its migrators with the
config, policy and coordination backend it boots with.

A `galleon` CLI may come later for tooling that is about the source rather
than a built app, such as the dev server and generators. If it offers
`galleon migrate`, it only forwards to the app's binary; it never
reimplements a command.

The commands are defined as data in package `cli`, which runs them, prints
their help, and generates the Reference › CLI page, so the three can't
differ. The package uses only the standard library, as the core module must.

## Consequences

- The command runs exactly the migrators, policy and lock the deploy boots
  with: there is no second definition to drift from it.
- It ships in the image that is deployed. A release step is
  `<app> migrate apply`, with no Go toolchain or source at deploy time.
- Every app's `main` must hand its arguments to `cli.Main`. An app that
  doesn't has no commands; it can still call `App.WithMigrations` itself.
- An app that parses its own flags passes `cli.Main` what is left after
  them, such as `flag.Args()`.
- Commands that need a running app's state rather than its wiring, such as
  ops endpoints, aren't commands of this kind; they wait for the security
  plane.

## Alternatives considered

- **A separate `galleon` CLI that builds and runs the app (`go run`).**
  Needs the Go toolchain and the source wherever migrations run, which a
  production image usually lacks, and the build it runs may not be the one
  deployed.
- **A separate CLI that calls a running app over an ops endpoint.** Needs a
  node up, and the security plane to protect the endpoint; it also can't
  apply before the first node of a release starts.
- **A CLI framework, such as cobra.** The core module depends only on the
  standard library and OpenTelemetry; four commands don't justify a
  dependency.

## Sources

- The build plan's step E1, and the question put to the owner in the
  project thread that settled it.
- 05 §2 (`cmd/galleon` in the module layout) and §7.7 ("one `galleon
  migrate` CLI").
