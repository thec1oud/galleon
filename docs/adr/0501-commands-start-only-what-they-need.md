# 0501. Start only what a command needs, and act on nothing at boot

Status: Proposed
Date: 2026-10-22

## Context

`migrate status`, `plan` and `apply` (0500) need the app's migrators built,
which means their providers started: a migrator over Postgres takes a
started `*pg.Manager`. `App.Start` would do that, but it also acts on the
policy's boot mode, so a deploy configured with `mode: apply` would apply
during `migrate plan`; it runs `OnStart` hooks, gates readiness, and starts
every provider, servers included. The only workaround before the command
existed was to run the plan in verify mode and hope the environment didn't
say otherwise.

## Decision

The kernel gets a second way to run an app, `App.WithMigrations(ctx, fn)`.
It starts only the providers the migrators' constructors take, and what
those depend on, in dependency order; builds the migrators and their
runner with the app's policy and coordination backend; calls `fn` with the
app's `Migrations`; and then stops what it started, in reverse, whatever
`fn` returned. It never acts on the boot mode, runs no hooks, polls no
readiness gate and starts nothing else.

Like `Start`, it runs once: the app can't start afterwards.

## Consequences

- `migrate plan` and `migrate status` never change the database, whatever
  the config's mode, and `migrate apply` applies only because it was asked.
- A command doesn't open the HTTP port or run a module's start-up work, so
  it can run beside the serving nodes.
- The policy and the lock are unchanged: a command is refused what boot
  would be refused, and waits for the same lock.
- A migrator's constructor must take what it needs as parameters, which
  the seam already required; one that reached for a provider some other
  way would find it unstarted. A provider that matters only for its side
  effect doesn't start either: with a Manager supplied by `pg.Use`, the
  app's telemetry isn't attached to it during a command.
- While a command runs, `Resolve` returns only what it started, `Ready`
  fails and `Shutdown` is refused; when it returns, `App.Migrations()`
  refuses too.
- Planning offline still starts the migrators' providers, so the database
  must be reachable. A migrator that can plan offline but whose
  constructor needs a pool can't plan in CI without a database; that would
  need a separate offline constructor.

## Alternatives considered

- **Start the whole app with the mode forced to verify.** Still runs hooks
  and servers, and a node that starts a server for a plan is surprising.
- **A config option or role that turns boot off.** Puts a command's
  concern into every deploy's config, and is easy to leave set.
- **Build the migrators without starting their providers.** Constructors
  may rely on started dependencies, such as a connected pool.

## Sources

- 0019, the migration seam, and 0010, validating everything at boot,
  which `galleon.New` still does for a command.
- The "Deploy migrations safely" guide before this change, which warned
  never to make the plan with mode apply set.
