// Package cli runs a Galleon app from its own command line. Without a
// command it serves; `migrate status`, `migrate plan` and `migrate apply`
// act on the app's migrators with the config, policy and coordination
// backend the app boots with. A main function hands it its arguments:
//
//	func main() {
//		os.Exit(cli.Main(context.Background(), os.Args[1:], options()...))
//	}
//
// The commands are subcommands of the app's binary, not a separate tool,
// since only the app's binary has its migrators compiled in.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/migrate"
)

// The exit codes of every command.
const (
	// ExitOK: the command did what it was asked.
	ExitOK = 0
	// ExitFailed: the app didn't boot, or the command failed, was refused
	// by the policy, or found a migrator not ready.
	ExitFailed = 1
	// ExitUsage: the command line is wrong. Usage goes to stderr.
	ExitUsage = 2
)

// Command describes one command, as its help and the CLI reference show
// it. Commands returns them; the definitions run them, so the two can't
// differ.
type Command struct {
	// Name is the command as typed, such as "migrate plan". Without a
	// command, the binary runs "serve".
	Name string
	// Args is the synopsis after the name, such as "[-offline]".
	Args string
	// Summary is one line on what the command does.
	Summary string
	// Doc is the rest of its description, in paragraphs separated by a
	// blank line.
	Doc string
	// Flags are the command's flags, in the order its help lists them.
	Flags []Flag
	// Exits says what each exit code means for this command.
	Exits []Exit
}

// Flag is a command's flag, as the flag package parses it.
type Flag struct {
	Name, Default, Usage string
}

// Exit is what an exit code means for one command.
type Exit struct {
	Code    int
	Meaning string
}

type command struct {
	Command // its Flags are left empty: flags declares them
	// flags declares the command's flags on fs, into v; it may be nil.
	flags func(fs *flag.FlagSet, v *values)
	run   func(ctx context.Context, e *env) int
}

// values are the parsed flags of every command.
type values struct {
	offline bool
}

// env is what a command runs with.
type env struct {
	stdout, stderr io.Writer
	opts           []galleon.Option
	values
}

// flagSet returns c's flags, parsing into v.
func (c *command) flagSet(v *values) *flag.FlagSet {
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if c.flags != nil {
		c.flags(fs, v)
	}
	return fs
}

// describe returns c's description, with its flags as it declares them.
func (c *command) describe() Command {
	d := c.Command
	d.Exits = slices.Clone(c.Exits)
	d.Flags = nil
	c.flagSet(new(values)).VisitAll(func(f *flag.Flag) {
		d.Flags = append(d.Flags, Flag{Name: f.Name, Default: f.DefValue, Usage: f.Usage})
	})
	return d
}

var commands = []*command{
	{
		Command: Command{
			Name:    "serve",
			Summary: "Run the app: start it, serve until SIGINT or SIGTERM, then drain and stop.",
			Doc:     "This is what the binary does without a command. At boot the app acts on its migrators as the `migrate` config section's `mode` says: `off`, `verify` (the `Production` default) or `apply`.",
			Exits: []Exit{
				{ExitOK, "the app stopped cleanly"},
				{ExitFailed, "the app didn't boot or start, failed while running, or didn't stop cleanly"},
			},
		},
		run: serve,
	},
	{
		Command: Command{
			Name:    "migrate status",
			Summary: "Print each migrator's status: its state, whether the policy counts it ready, its drift and its background tasks.",
			Doc:     "Reads the database and changes nothing. It exits 1 when any migrator isn't ready, so a deploy can check the schema before it sends traffic.",
			Exits: []Exit{
				{ExitOK, "every migrator is ready"},
				{ExitFailed, "the app didn't boot, a status couldn't be read, or a migrator isn't ready"},
			},
		},
		run: migrateStatus,
	},
	{
		Command: Command{
			Name:    "migrate plan",
			Args:    "[-offline]",
			Summary: "Print what migrate apply would do: each migrator's pending steps, their risks, the plan's fingerprint, and whether the policy would refuse it.",
			Doc: "It changes nothing. Approve a plan by pinning its fingerprint in the `migrate` section's `approve` key, or `<APP>_MIGRATE_APPROVE`, as `name=fingerprint` pairs.\n\n" +
				"With `-offline`, a migrator that can plan without a database plans every change as if the database were empty, and one that can't says so. The providers the migrators need still start, so the database must be reachable even then.",
			Exits: []Exit{
				{ExitOK, "every migrator planned, and the policy allows every plan"},
				{ExitFailed, "the app didn't boot, a plan couldn't be made, a status couldn't be read, or the policy would refuse a plan"},
			},
		},
		flags: func(fs *flag.FlagSet, v *values) {
			fs.BoolVar(&v.offline, "offline", false, "plan without reading the database's state")
		},
		run: migratePlan,
	},
	{
		Command: Command{
			Name:    "migrate apply",
			Summary: "Apply every migrator's pending changes, in order, each under its coordination lock, and stop at the first failure.",
			Doc: "The policy still applies: the risk ceiling, unknown risk and approved fingerprints refuse a plan here as they do at boot. The `mode` doesn't: `migrate apply` applies whatever it is.\n\n" +
				"It waits for the lock as `lock_wait` says, and has no time limit of its own: SIGINT or SIGTERM cancels it. With `coord/local`, whose lock holds within one process, don't run it while a node applies at boot.",
			Exits: []Exit{
				{ExitOK, "every migrator is applied, or had nothing pending"},
				{ExitFailed, "the app didn't boot, or an apply failed or was refused"},
			},
		},
		run: migrateApply,
	},
}

// ExitCodes returns what each exit code means, for every command; each
// command's Exits says what it means for that one.
func ExitCodes() []Exit {
	return []Exit{
		{ExitOK, "the command did what it was asked"},
		{ExitFailed, "the app didn't boot, or the command failed, was refused by the policy, or found a migrator not ready"},
		{ExitUsage, "the command line is wrong; usage goes to stderr"},
	}
}

// Commands returns every command's description, in the order help lists
// them.
func Commands() []Command {
	out := make([]Command, len(commands))
	for i, c := range commands {
		out[i] = c.describe()
	}
	return out
}

// Main runs the command args name, as os.Args[1:] gives it, on the app opts
// build, and returns the exit code. Without a command it serves. Reports go
// to stdout, errors and usage to stderr, and the app's logs wherever its
// logger writes them.
func Main(ctx context.Context, args []string, opts ...galleon.Option) int {
	return Run(ctx, args, os.Stdout, os.Stderr, opts...)
}

// Run is Main writing to stdout and stderr, such as for a test.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, opts ...galleon.Option) int {
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		if len(args) == 1 {
			usage(stdout, "")
			return ExitOK
		}
		if c, rest := find(args[1:]); c != nil && len(rest) == 0 {
			commandUsage(stdout, c)
			return ExitOK
		}
		if len(args) == 2 && group(args[1]) {
			usage(stdout, args[1])
			return ExitOK
		}
		fmt.Fprintf(stderr, "unknown command %q\n\n", strings.Join(args[1:], " "))
		usage(stderr, "")
		return ExitUsage
	}
	c, rest := find(args)
	if c == nil {
		if group(args[0]) {
			if len(args) > 1 && isHelp(args[1]) {
				usage(stdout, args[0])
				return ExitOK
			}
			fmt.Fprintf(stderr, "%s needs a subcommand\n\n", args[0])
			usage(stderr, args[0])
			return ExitUsage
		}
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		usage(stderr, "")
		return ExitUsage
	}
	var v values
	fs := c.flagSet(&v)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			commandUsage(stdout, c)
			return ExitOK
		}
		fmt.Fprintf(stderr, "%s: %v\n\n", c.Name, err)
		commandUsage(stderr, c)
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n\n", c.Name, fs.Arg(0))
		commandUsage(stderr, c)
		return ExitUsage
	}
	return c.run(ctx, &env{stdout: stdout, stderr: stderr, opts: opts, values: v})
}

func isHelp(arg string) bool { return arg == "-h" || arg == "-help" || arg == "--help" }

// group reports whether word starts the names of several commands, as
// "migrate" does.
func group(word string) bool {
	return slices.ContainsFunc(commands, func(c *command) bool { return strings.HasPrefix(c.Name, word+" ") })
}

// find returns the command args start with, and the arguments after its
// name.
func find(args []string) (*command, []string) {
	for _, c := range commands {
		words := strings.Fields(c.Name)
		if len(args) >= len(words) && slices.Equal(args[:len(words)], words) {
			return c, args[len(words):]
		}
	}
	return nil, nil
}

// program is the binary's name, as usage shows it.
func program() string {
	if len(os.Args) == 0 || os.Args[0] == "" {
		return "app"
	}
	return filepath.Base(os.Args[0])
}

// usage lists the commands, or only those of group when it isn't "".
func usage(w io.Writer, group string) {
	var b strings.Builder
	if group == "" {
		fmt.Fprintf(&b, "Usage: %s [command] [flags]\n\nCommands:\n", program())
	} else {
		fmt.Fprintf(&b, "Usage: %s %s <command> [flags]\n\nCommands:\n", program(), group)
	}
	for _, c := range commands {
		if group == "" || strings.HasPrefix(c.Name, group+" ") {
			fmt.Fprintf(&b, "  %-15s %s\n", c.Name, c.Summary)
		}
	}
	fmt.Fprintf(&b, "\nWithout a command, the app serves. Run `%s help <command>` for a command's flags and exit codes.\n", program())
	io.WriteString(w, b.String())
}

func commandUsage(w io.Writer, c *command) {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: %s %s", program(), c.Name)
	if c.Args != "" {
		fmt.Fprintf(&b, " %s", c.Args)
	}
	fmt.Fprintf(&b, "\n\n%s\n", c.Summary)
	if c.Doc != "" {
		fmt.Fprintf(&b, "\n%s\n", c.Doc)
	}
	if d := c.describe(); len(d.Flags) > 0 {
		b.WriteString("\nFlags:\n")
		for _, f := range d.Flags {
			fmt.Fprintf(&b, "  -%s\t%s (default %s)\n", f.Name, f.Usage, f.Default)
		}
	}
	b.WriteString("\nExit codes:\n")
	for _, e := range c.Exits {
		fmt.Fprintf(&b, "  %d  %s\n", e.Code, e.Meaning)
	}
	io.WriteString(w, b.String())
}

func serve(ctx context.Context, e *env) int {
	app, err := galleon.New(e.opts...)
	if err != nil {
		fmt.Fprintln(e.stderr, err)
		return ExitFailed
	}
	if err := app.Run(ctx); err != nil {
		fmt.Fprintln(e.stderr, err)
		return ExitFailed
	}
	return ExitOK
}

// withMigrations builds the app and runs fn on its migrations, cancelled
// by SIGINT or SIGTERM. fn returns the exit code; an error from booting,
// starting or stopping makes it ExitFailed.
func withMigrations(ctx context.Context, e *env, fn func(ctx context.Context, m *galleon.Migrations) (int, error)) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The first signal cancels the command; a second, while it stops what
	// it started, kills the process as signals do by default.
	go func() { <-ctx.Done(); stop() }()
	app, err := galleon.New(e.opts...)
	if err != nil {
		fmt.Fprintln(e.stderr, err)
		return ExitFailed
	}
	code := ExitOK
	err = app.WithMigrations(ctx, func(ctx context.Context, m *galleon.Migrations) error {
		var err error
		code, err = fn(ctx, m)
		return err
	})
	if err != nil {
		fmt.Fprintln(e.stderr, err)
		return ExitFailed
	}
	return code
}

func migrateStatus(ctx context.Context, e *env) int {
	return withMigrations(ctx, e, func(ctx context.Context, m *galleon.Migrations) (int, error) {
		reports, err := m.Status(ctx)
		if err != nil {
			return ExitFailed, err
		}
		if err := migrate.WriteStatus(e.stdout, reports); err != nil {
			return ExitFailed, err
		}
		for _, r := range reports {
			if r.Err != nil || r.Ready != nil {
				return ExitFailed, nil
			}
		}
		return ExitOK, nil
	})
}

func migratePlan(ctx context.Context, e *env) int {
	return withMigrations(ctx, e, func(ctx context.Context, m *galleon.Migrations) (int, error) {
		plans, err := m.Plan(ctx, e.offline)
		if err != nil {
			return ExitFailed, err
		}
		if err := migrate.WritePlans(e.stdout, plans); err != nil {
			return ExitFailed, err
		}
		var failed []string
		for _, p := range plans {
			if p.Err != nil || p.StatusErr != nil || p.Refused != nil {
				failed = append(failed, p.Name)
			}
		}
		if len(failed) > 0 {
			fmt.Fprintf(e.stderr, "migrate plan: %s can't be applied as planned; the plan above says why\n", strings.Join(failed, ", "))
			return ExitFailed, nil
		}
		return ExitOK, nil
	})
}

func migrateApply(ctx context.Context, e *env) int {
	return withMigrations(ctx, e, func(ctx context.Context, m *galleon.Migrations) (int, error) {
		reports, err := m.Apply(ctx)
		if werr := migrate.WriteApply(e.stdout, reports); werr != nil {
			return ExitFailed, errors.Join(err, werr)
		}
		if err != nil {
			// The report already says why the migrator that stopped the
			// apply failed; an error none of them carries is said here.
			for _, r := range reports {
				if r.Err != nil {
					fmt.Fprintf(e.stderr, "migrate apply: migrator %q: %s; the report above says why\n", r.Name, r.Outcome)
					return ExitFailed, nil
				}
			}
			return ExitFailed, err
		}
		return ExitOK, nil
	})
}
