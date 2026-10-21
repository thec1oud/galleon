// Package dpg is the migration seam's adapter for DPG, the declarative
// PostgreSQL schema tool: a migrate.Migrator that applies the migrations a
// DPG project's migration log records to one of its databases.
//
//	galleon.Migrator(r, "schema", func(m *pg.Manager) (migrate.Migrator, error) {
//		return dpg.New("schema", "db", m.Pool(""))
//	})
//
// The project is the directory a developer runs dpg in; its source must be
// recorded in its log (dpg plan --record, checked in CI by dpg plan
// --check), since the adapter only ever runs what the log records and never
// writes to the project. It runs DPG's own apply path through DPG's library
// engine (github.com/thec1oud/dpg/pkg/dpg/engine), so a database is migrated
// exactly as dpg apply would migrate it: each migration in its transaction
// with its record in DPG's ledger, DPG's preflight checks, and a migration
// whose steps after its transaction failed finished first.
//
// One migration of the log is one step of the seam. Its risk is that of
// its riskiest statement: DPG's Safe, Caution and Destructive are the
// seam's; a statement DPG leaves for a person to do is Manual; anything
// else is Unknown, never Safe. A plan's fingerprint covers each migration's
// ID and every statement's stage, safety and SQL.
//
// Every migration is fenced: the hook DPG's engine runs first in each of a
// migration's transactions raises the migrator's row of a pg.Fence, kept in
// DPG's ledger schema (which DPG ignores), to the lease's epoch and token,
// so the fence commits or rolls back with the migration, and a node that
// paused past its lease can't apply anything once it resumes (CodeStaleToken,
// CodeStaleEpoch). A statement DPG runs outside a transaction, a new enum
// value before a migration's transaction or a CREATE INDEX CONCURRENTLY
// after it, is checked against the fence in a transaction of its own just
// before it runs, and so is each of DPG's own records around it. A holder
// that pauses between that check and the statement can still run it after
// its lease has passed to another node. Nothing is applied twice even
// then, but not every such statement is safe to repeat: the enum values
// are ADD VALUE IF NOT EXISTS, and grants repeat harmlessly, but DPG writes
// CREATE INDEX CONCURRENTLY without IF NOT EXISTS, so a second run fails
// ("already exists"). The migration's transaction has committed by then,
// and DPG runs the statements after it again until they all succeed, so a
// person must drop the index for the next apply to finish the migration;
// the same goes for an index whose build failed part way, which PostgreSQL
// leaves behind, invalid. See README.md.
//
// Status, Plan and Verify only read. Each call opens a connection of its
// own from the pool's configuration, and closes it, rather than borrowing
// one of the pool's: a migration may change session state (SET ROLE, to
// create an object as its owner), which must not reach the app's
// connections. The pool's AfterConnect and BeforeAcquire hooks don't run
// on it.
//
// The migrator is a migrate.Planner, OfflinePlanner and Verifier. It isn't
// a Backgrounder: DPG has no work that continues after a migration.
//
// DPG's engine is provisional, and this adapter with it: see README.md.
package dpg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thec1oud/dpg/pkg/dpg/engine"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
	"github.com/thec1oud/galleon/pg"
)

// Error codes of the errors Apply returns.
const (
	// CodeStaleToken: a holder with a higher fencing token has applied
	// since this lease was granted.
	CodeStaleToken = "dpg.stale_token"
	// CodeStaleEpoch: a holder of a later coordination epoch has applied
	// since this lease was granted.
	CodeStaleEpoch = "dpg.stale_epoch"
	// CodeNoLock: Apply was called without a lease.
	CodeNoLock = "dpg.no_lock"
	// CodeNotPending: the database isn't Pending, so there's nothing Apply
	// may do.
	CodeNotPending = "dpg.not_pending"
)

// FenceTable is the fencing table's name, in DPG's ledger schema.
const FenceTable = "galleon_fence"

// Option configures a Migrator.
type Option func(*Migrator)

// Database names the project's cluster and database to migrate; either may
// be empty when the project has only one.
func Database(cluster, database string) Option {
	return func(m *Migrator) { m.cluster, m.database = cluster, database }
}

// Migrator applies a DPG project's migration log to one database.
type Migrator struct {
	name              string
	dir               string
	cluster, database string
	pool              *pgxpool.Pool
	offline           migrate.Plan
}

var (
	_ migrate.Migrator       = (*Migrator)(nil)
	_ migrate.Planner        = (*Migrator)(nil)
	_ migrate.OfflinePlanner = (*Migrator)(nil)
	_ migrate.Verifier       = (*Migrator)(nil)
)

// New opens the DPG project in dir now, so that a project that doesn't
// compile, or whose source has changes its log doesn't record, fails at
// boot, and returns the migrator named name, on pool's database.
func New(name, dir string, pool *pgxpool.Pool, opts ...Option) (*Migrator, error) {
	if err := migrate.ValidName(name); err != nil {
		return nil, fmt.Errorf("dpg: %w", err)
	}
	m := &Migrator{name: name, dir: dir, pool: pool}
	for _, o := range opts {
		o(m)
	}
	e, err := m.engine(nil)
	if err != nil {
		return nil, err
	}
	m.offline = planOf(e.PlanOffline().Steps)
	return m, nil
}

func (m *Migrator) Name() string { return m.name }

// engine opens the project on conn (offline when it is nil).
func (m *Migrator) engine(conn *pgx.Conn) (*engine.Engine, error) {
	e, err := engine.Open(engine.Config{Dir: m.dir, Cluster: m.cluster, Database: m.database, Conn: conn})
	if err != nil {
		return nil, fmt.Errorf("dpg: migrator %q: %w", m.name, err)
	}
	return e, nil
}

// session runs f on an engine over a connection of its own.
func (m *Migrator) session(ctx context.Context, f func(*engine.Engine, *pgx.Conn) error) error {
	conn, err := pgx.ConnectConfig(ctx, m.pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("dpg: migrator %q: connecting: %w", m.name, err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	e, err := m.engine(conn)
	if err != nil {
		return err
	}
	defer e.Close(context.WithoutCancel(ctx))
	return f(e, conn)
}

func version(id string) string {
	if id == "" {
		return "none"
	}
	return id
}

func statusOf(s engine.Status) migrate.Status {
	out := migrate.Status{Expected: version(s.Expected), Current: version(s.Current), Detail: s.Detail}
	switch s.State {
	case engine.UpToDate:
		out.State = migrate.UpToDate
	case engine.Pending:
		out.State = migrate.Pending
	case engine.Ahead:
		out.State = migrate.Ahead
	default:
		// A state this adapter doesn't know is never taken for one it
		// may apply over.
		out.State = migrate.Drifted
		if out.Detail == "" {
			out.Detail = "dpg reports the database as " + s.State.String()
		}
	}
	return out
}

// Status compares the migrations DPG's ledger records with the log.
func (m *Migrator) Status(ctx context.Context) (migrate.Status, error) {
	var st migrate.Status
	err := m.session(ctx, func(e *engine.Engine, _ *pgx.Conn) error {
		s, err := e.Status(ctx)
		st = statusOf(s)
		return err
	})
	if err != nil {
		return migrate.Status{}, m.wrap(err)
	}
	return st, nil
}

// Verify lists every way the database's objects differ from the snapshot
// at its place in the log, as dpg verify does: only the findings dpg
// verify fails for, not unmanaged objects or warnings. A database whose
// last migration isn't in the log is a difference too.
func (m *Migrator) Verify(ctx context.Context) (migrate.Drift, error) {
	var d migrate.Drift
	err := m.session(ctx, func(e *engine.Engine, _ *pgx.Conn) error {
		fs, err := e.Verify(ctx)
		for _, f := range fs {
			if f.Failing {
				d.Differences = append(d.Differences, f.ID+": "+f.Detail)
			}
		}
		return err
	})
	if err != nil {
		return migrate.Drift{}, m.wrap(err)
	}
	return d, nil
}

// Plan lists the migrations the database hasn't run, and one whose steps
// after its transaction are still to be finished, with their risks.
func (m *Migrator) Plan(ctx context.Context) (migrate.Plan, error) {
	var pl migrate.Plan
	err := m.session(ctx, func(e *engine.Engine, _ *pgx.Conn) error {
		p, err := e.Plan(ctx)
		pl = planOf(p.Steps)
		return err
	})
	if err != nil {
		return nil, m.wrap(err)
	}
	return pl, nil
}

// PlanOffline plans every migration of the log, as if the database were
// empty, without connecting: what CI reviews. Its fingerprint isn't the
// online plan's, which covers only what the database hasn't run; approve
// the online one.
func (m *Migrator) PlanOffline(context.Context) (migrate.Plan, error) {
	return m.offline, nil
}

// riskOf maps a DPG statement to the seam's risk.
func riskOf(s engine.Step) migrate.Risk {
	if s.Mode == engine.Instruction {
		return migrate.Manual
	}
	switch s.Safety {
	case engine.Safe:
		return migrate.Safe
	case engine.Caution:
		return migrate.Caution
	case engine.Destructive:
		return migrate.Destructive
	}
	return migrate.Unknown
}

// planOf groups DPG's statements into a step per migration, in order.
func planOf(statements []engine.Step) migrate.Plan {
	var (
		steps   []migrate.StepInfo
		content [][]byte
		counts  []map[engine.Mode]int
	)
	for _, s := range statements {
		if n := len(steps); n == 0 || steps[n-1].ID != s.Migration {
			// A migration starts as Safe only because it has a statement
			// of its own to classify; riskOf decides from there.
			steps = append(steps, migrate.StepInfo{ID: s.Migration, Risk: migrate.Safe})
			content = append(content, nil)
			counts = append(counts, map[engine.Mode]int{})
		}
		i := len(steps) - 1
		if r := riskOf(s); r.Above(steps[i].Risk) {
			steps[i].Risk = r
		}
		counts[i][s.Mode]++
		content[i] = fmt.Appendf(content[i], "%s\x00%s\x00%s\x00", s.Mode, s.Safety, s.SQL)
	}
	for i := range steps {
		steps[i].Description = describe(steps[i].ID, counts[i])
	}
	return migrate.NewPlan(migrate.Fingerprint(steps, content), steps)
}

// describe says what a migration runs, without its SQL.
func describe(id string, n map[engine.Mode]int) string {
	var parts []string
	for _, k := range []struct {
		mode engine.Mode
		what string
	}{
		{engine.BeforeTransaction, "before its transaction"},
		{engine.InTransaction, "in its transaction"},
		{engine.AfterTransaction, "after its transaction"},
		{engine.Instruction, "for a person to do"},
	} {
		if n[k.mode] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[k.mode], k.what))
		}
	}
	return fmt.Sprintf("dpg migration %s (statements: %s)", id, strings.Join(parts, ", "))
}

// Apply runs the migrations the database hasn't run, in order, each in its
// transaction with its record in DPG's ledger and the fencing check. It
// refuses, before applying anything, a plan other than opts.Fingerprint or
// a migration the options don't allow, and a database that is Drifted or
// Ahead.
func (m *Migrator) Apply(ctx context.Context, opts migrate.ApplyOptions) (migrate.Result, error) {
	if opts.Lock == nil {
		return migrate.Result{}, errs.New(errs.FailedPrecondition, CodeNoLock, fmt.Sprintf("dpg: migrator %q never applies without a lease", m.name))
	}
	var res migrate.Result
	err := m.session(ctx, func(e *engine.Engine, conn *pgx.Conn) error {
		// Everything that can refuse does so before anything is created.
		s, err := e.Status(ctx)
		if err != nil {
			return err
		}
		if st := statusOf(s); st.State == migrate.Drifted || st.State == migrate.Ahead {
			return errs.New(errs.FailedPrecondition, CodeNotPending, fmt.Sprintf("dpg: migrator %q is %s, so there's nothing it may apply: %s", m.name, st.State, st.Detail))
		}
		p, err := e.Plan(ctx)
		if err != nil {
			return err
		}
		pl := planOf(p.Steps)
		if opts.Fingerprint != "" && opts.Fingerprint != pl.Fingerprint() {
			return errs.New(errs.FailedPrecondition, migrate.CodeFingerprintMismatch, fmt.Sprintf("dpg: migrator %q: the pending plan's fingerprint is %s, not %s as approved; nothing was applied", m.name, pl.Fingerprint(), opts.Fingerprint))
		}
		byID := map[string]migrate.StepInfo{}
		for _, st := range pl.Steps() {
			if !opts.Allows(st.Risk) {
				return errs.New(errs.FailedPrecondition, migrate.CodeRiskRefused, fmt.Sprintf("dpg: migrator %q: migration %s is of %s risk, which isn't allowed (max %s, unknown %s); nothing was applied", m.name, st.ID, st.Risk, migrate.Ceiling(opts.MaxRisk), allowed(opts.AllowUnknown)))
			}
			byID[st.ID] = st
		}
		if len(byID) == 0 {
			return nil
		}
		fence, err := m.ensureFence(ctx, conn, e.LedgerSchema())
		if err != nil {
			return err
		}
		lease := pg.FenceLease{Epoch: opts.Lock.Epoch(), Token: int64(opts.Lock.Token())}
		// DPG plans again as it applies; under the lock that is the plan
		// above, but a migration that isn't in it is still reported, as
		// of Unknown risk.
		info := func(id string) migrate.StepInfo {
			if st, ok := byID[id]; ok {
				return st
			}
			return migrate.StepInfo{ID: id, Description: "dpg migration " + id, Risk: migrate.Unknown}
		}
		r, err := e.Apply(ctx, engine.ApplyOptions{
			// DPG's own gate is set to what the options allow too, so it
			// can't run more than they do whatever it plans.
			MaxSafety:    maxSafety(opts),
			Instructions: opts.Allows(migrate.Manual),
			Hook: func(ctx context.Context, tx pgx.Tx) error {
				return m.stale(fence.Raise(ctx, tx, m.name, lease))
			},
			Observe: func(ctx context.Context, id string) (context.Context, func(error)) {
				return opts.Step(ctx, info(id))
			},
		})
		for _, id := range r.Applied {
			res.Applied = append(res.Applied, info(id))
		}
		return err
	})
	if err != nil {
		return res, m.wrap(err)
	}
	return res, nil
}

// maxSafety is the most dangerous DPG statement opts allow.
func maxSafety(opts migrate.ApplyOptions) engine.Safety {
	switch {
	case opts.Allows(migrate.Destructive):
		return engine.Destructive
	case opts.Allows(migrate.Caution):
		return engine.Caution
	}
	return engine.Safe
}

func allowed(b bool) string {
	if b {
		return "allowed"
	}
	return "refused"
}

// ensureFence creates the fencing table in DPG's ledger schema, under a
// transaction-scoped advisory lock so that migrators sharing it don't race
// creating it. DPG creates the schema the same way, IF NOT EXISTS, when it
// first records a migration.
func (m *Migrator) ensureFence(ctx context.Context, conn *pgx.Conn, schema string) (pg.Fence, error) {
	f := pg.Fence{Table: pgx.Identifier{schema, FenceTable}}
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", f.Table.Sanitize()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
			return err
		}
		return f.Create(ctx, tx)
	})
	if err != nil {
		return pg.Fence{}, fmt.Errorf("creating %s: %w", f.Table.Sanitize(), err)
	}
	return f, nil
}

// stale gives a stale lease the adapter's codes.
func (m *Migrator) stale(err error) error {
	var s *pg.StaleLeaseError
	switch {
	case !errors.As(err, &s):
		return err
	case s.Superseded:
		return errs.New(errs.FailedPrecondition, CodeStaleEpoch, fmt.Sprintf("dpg: migrator %q: the lease's epoch %q is stale: a holder of epoch %q has applied since", m.name, s.Lease.Epoch, s.Held.Epoch))
	default:
		return errs.New(errs.FailedPrecondition, CodeStaleToken, fmt.Sprintf("dpg: migrator %q: the lease's fencing token %d is stale: a holder of token %d in its epoch has applied since", m.name, s.Lease.Token, s.Held.Token))
	}
}

// wrap returns the adapter's own errors, wherever DPG's engine wrapped
// them, as they are, and names the migrator in the others.
func (m *Migrator) wrap(err error) error {
	if e := (*errs.Error)(nil); errors.As(err, &e) {
		return e
	}
	if strings.HasPrefix(err.Error(), "dpg: ") {
		return err
	}
	return fmt.Errorf("dpg: migrator %q: %w", m.name, err)
}
