// Package sqlmigrate is Galleon's built-in migrator: numbered SQL files,
// applied in version order, on PostgreSQL through pgx.
//
//	//go:embed migrations/*.sql
//	var files embed.FS
//
//	galleon.Migrator(r, "schema", func(m *pg.Manager) (migrate.Migrator, error) {
//		sub, _ := fs.Sub(files, "migrations")
//		return sqlmigrate.New("schema", sub, m.Pool(""))
//	})
//
// Files are named <version>_<name>.sql, such as 0001_create_orders.sql,
// at the root of the fs.FS; the version is the leading number, unique per
// migrator. Other files are ignored, but a .sql file that isn't named so is
// an error, so a typo can't skip one.
//
// Comment lines at the top of a file, before its first line of SQL, may
// hold directives, written exactly so: "--", any spaces, "galleon:", the
// directive, and for risk one space and the value.
//
//	-- galleon:risk caution
//	-- galleon:no-transaction
//
// A directive given twice, an unknown directive or value, and a near miss
// (Galleon:, galleon: risk, a tab) are errors, never silently ignored.
//
// "risk" declares the file's risk (safe, caution, destructive or manual),
// which plans report and the deploy policy checks. Without it a file is of
// Unknown risk: sqlmigrate doesn't guess from the SQL. "no-transaction"
// runs the file outside a transaction, for statements that can't run in
// one, such as CREATE INDEX CONCURRENTLY. Such a file should hold a single
// statement: PostgreSQL runs several sent at once as one implicit
// transaction anyway. If it fails half way, or the node dies before it is
// recorded, what it did stays and it is pending again, so write it to be
// safe to repeat (IF NOT EXISTS). Its statement runs outside the fence: the
// fence is checked in a transaction just before it, and again when it is
// recorded, but a holder that pauses between that check and the statement
// can run it after its lease has passed to another node, which may be
// running it too. Its record then fails, and the statement, being safe to
// repeat, does no harm.
//
// Applied versions are recorded, with each file's SHA-256, in a table
// (galleon_migrations by default, see Table), per migrator. A recorded file
// whose content has changed since makes the status Drifted, as does a
// recorded version this build lacks below its latest; recorded versions
// above its latest make it Ahead. A pending file older than the latest
// applied version (0002 when 0003 is applied, as when branches merge) makes
// it Drifted too, and isn't applied, unless the migrator is made with
// AllowOutOfOrder.
//
// Each file is applied in its own transaction, together with its record.
// The transaction first raises the migrator's fencing row (the table name
// with "_fence" appended) to the lease's epoch and token, and fails if the
// lease is stale: a node that paused past its lease, while another took the
// lock, can't apply anything once it resumes. Tokens compare only within
// one coordination epoch (coord.Lease.Epoch): the row refuses a lower token
// of its epoch (CodeStaleToken). A lease of a new epoch, such as one from a
// new process on the in-process backend, whose tokens start again at 1,
// takes the row over, and the epoch it replaces is recorded as superseded
// in the row; a lease of a superseded epoch is refused whatever its token
// (CodeStaleEpoch), so the old process can't take the row back. Epochs
// aren't ordered otherwise: the first lease of an epoch the row hasn't seen
// wins it. A cluster backend keeps one epoch for good; the in-process
// backend, whose epoch is per process, serves a single node, which the
// kernel's multi-node check enforces. A fencing table made by an earlier
// version, without the epoch or superseded columns, gains them when the
// migrator first runs.
//
// The migrator is a migrate.Planner, OfflinePlanner and Verifier.
package sqlmigrate

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/migrate"
)

// DB is what the migrator runs on: a *pgxpool.Pool, such as
// pg.Manager.Pool, or a *pgx.Conn.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Error codes of the errors Apply returns.
const (
	// CodeStaleToken: a holder with a higher fencing token has applied
	// since this lease was granted.
	CodeStaleToken = "sqlmigrate.stale_token"
	// CodeStaleEpoch: a holder of a later coordination epoch has applied
	// since this lease was granted.
	CodeStaleEpoch = "sqlmigrate.stale_epoch"
	// CodeNoLock: Apply was called without a lease.
	CodeNoLock = "sqlmigrate.no_lock"
	// CodeNotPending: the database isn't Pending, so there's nothing Apply
	// may do.
	CodeNotPending = "sqlmigrate.not_pending"
)

// Option configures a Migrator.
type Option func(*Migrator)

// Table sets the table applied versions are recorded in, optionally
// schema-qualified ("ops.migrations"). The default is galleon_migrations.
// Several migrators can share a table: rows are per migrator. The table's
// name, with "_fence" appended for the fencing table, must fit PostgreSQL's
// 63 bytes, so it is at most 57.
func Table(name string) Option {
	return func(m *Migrator) { m.table = pgx.Identifier(strings.Split(name, ".")) }
}

// AllowOutOfOrder applies a pending file older than the latest applied
// version, rather than calling the database Drifted.
func AllowOutOfOrder() Option { return func(m *Migrator) { m.outOfOrder = true } }

// Migrator applies a directory of numbered SQL files.
type Migrator struct {
	name       string
	db         DB
	files      []file
	table      pgx.Identifier
	outOfOrder bool
}

// maxName is PostgreSQL's longest identifier, in bytes.
const maxName = 63

func (m *Migrator) checkTable() error {
	if len(m.table) == 0 || len(m.table) > 2 {
		return fmt.Errorf("table %q must be a name, or schema.name", strings.Join(m.table, "."))
	}
	for _, part := range m.table {
		if part == "" {
			return fmt.Errorf("table %q has an empty part", strings.Join(m.table, "."))
		}
		if len(part) > maxName {
			return fmt.Errorf("table %q: %q is longer than %d bytes", strings.Join(m.table, "."), part, maxName)
		}
	}
	if last := m.table[len(m.table)-1]; len(last)+len(fenceSuffix) > maxName {
		return fmt.Errorf("table %q: the name must be at most %d bytes, so that %q fits", strings.Join(m.table, "."), maxName-len(fenceSuffix), last+fenceSuffix)
	}
	return nil
}

const fenceSuffix = "_fence"

type file struct {
	version  int64
	name     string // the file name
	sql      string
	checksum string
	risk     migrate.Risk
	noTx     bool
}

func (f file) id() string { return strings.TrimSuffix(f.name, ".sql") }

func (f file) step() migrate.StepInfo {
	desc := f.name
	if f.noTx {
		desc += " (outside a transaction)"
	}
	return migrate.StepInfo{ID: f.id(), Description: desc, Risk: f.risk}
}

var (
	_ migrate.Migrator       = (*Migrator)(nil)
	_ migrate.Planner        = (*Migrator)(nil)
	_ migrate.Verifier       = (*Migrator)(nil)
	_ migrate.OfflinePlanner = (*Migrator)(nil)
)

var fileName = regexp.MustCompile(`^([0-9]+)_[^/]+\.sql$`)

// nearMiss is a comment that was meant as a directive: "galleon:" in
// another case, or with space before the colon.
var nearMiss = regexp.MustCompile(`(?i)^galleon\s*:`)

// New reads every migration in fsys now, so a bad file fails at boot, and
// returns the migrator named name, on db.
func New(name string, fsys fs.FS, db DB, opts ...Option) (*Migrator, error) {
	if err := migrate.ValidName(name); err != nil {
		return nil, fmt.Errorf("sqlmigrate: %w", err)
	}
	m := &Migrator{name: name, db: db, table: pgx.Identifier{"galleon_migrations"}}
	for _, o := range opts {
		o(m)
	}
	if err := m.checkTable(); err != nil {
		return nil, fmt.Errorf("sqlmigrate: migrator %q: %w", name, err)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("sqlmigrate: reading migrations: %w", err)
	}
	seen := map[int64]string{}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		match := fileName.FindStringSubmatch(e.Name())
		if match == nil {
			errs = append(errs, fmt.Errorf("%s: a migration must be named <version>_<name>.sql", e.Name()))
			continue
		}
		v, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || v <= 0 {
			errs = append(errs, fmt.Errorf("%s: the version must be a positive number", e.Name()))
			continue
		}
		if prev, ok := seen[v]; ok {
			errs = append(errs, fmt.Errorf("%s: version %d is also %s", e.Name(), v, prev))
			continue
		}
		seen[v] = e.Name()
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		f := file{version: v, name: e.Name(), sql: string(b)}
		sum := sha256.Sum256(b)
		f.checksum = hex.EncodeToString(sum[:])
		if err := f.directives(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		m.files = append(m.files, f)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("sqlmigrate: migrator %q: %w", name, err)
	}
	slices.SortFunc(m.files, func(a, b file) int { return cmp.Compare(a.version, b.version) })
	return m, nil
}

// directives reads the galleon: comment lines at the top of the file. It
// stops at the first line that isn't a comment, without reading further,
// however long that line is.
func (f *file) directives() error {
	f.risk = migrate.Unknown
	seen := map[string]bool{}
	rest := f.sql
	for rest != "" {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		comment, ok := strings.CutPrefix(line, "--")
		if !ok {
			return nil // the SQL has begun
		}
		comment = strings.TrimLeft(comment, " \t")
		d, ok := strings.CutPrefix(comment, "galleon:")
		if !ok {
			if nearMiss.MatchString(comment) {
				return fmt.Errorf("%q looks like a directive, but directives are written \"-- galleon:<directive>\", in lower case", line)
			}
			continue
		}
		if strings.ContainsAny(d, "\t\r") || strings.HasPrefix(d, " ") || strings.Contains(d, "  ") {
			return fmt.Errorf("%q: separate a directive and its value with one space, no tabs", line)
		}
		key, value, _ := strings.Cut(d, " ")
		if seen[key] {
			return fmt.Errorf("directive galleon:%s is given twice", key)
		}
		seen[key] = true
		switch key {
		case "risk":
			if value == "" {
				return errors.New("directive galleon:risk needs a value: safe, caution, destructive or manual")
			}
			r, err := migrate.ParseRisk(value)
			if err != nil || r.String() != value {
				return fmt.Errorf("directive galleon:risk %s: want safe, caution, destructive or manual, in lower case", value)
			}
			f.risk = r
		case "no-transaction":
			if value != "" {
				return fmt.Errorf("directive galleon:no-transaction takes no value, not %q", value)
			}
			f.noTx = true
		default:
			return fmt.Errorf("unknown directive galleon:%s (want risk or no-transaction)", key)
		}
	}
	return nil
}

func (m *Migrator) Name() string { return m.name }

// record is an applied version, as the table has it.
type record struct {
	version  int64
	name     string
	checksum string
}

// applied reads the migrator's records; none when the table doesn't exist
// yet, since Status never creates anything.
func (m *Migrator) applied(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}) ([]record, error) {
	var exists bool
	rows, err := q.Query(ctx, "SELECT to_regclass($1) IS NOT NULL", m.table.Sanitize())
	if err != nil {
		return nil, err
	}
	if exists, err = pgx.CollectExactlyOneRow(rows, pgx.RowTo[bool]); err != nil || !exists {
		return nil, err
	}
	rows, err = q.Query(ctx, "SELECT version, name, checksum FROM "+m.table.Sanitize()+" WHERE migrator = $1 ORDER BY version", m.name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (record, error) {
		var rec record
		err := r.Scan(&rec.version, &rec.name, &rec.checksum)
		return rec, err
	})
}

// compare is the database against the build.
type compare struct {
	pending []file
	drift   []string // edited, or missing below the build's latest
	ahead   []string // beyond the build's latest
	current int64
}

func (m *Migrator) compare(recs []record) compare {
	var c compare
	byVersion := map[int64]record{}
	for _, r := range recs {
		byVersion[r.version] = r
		c.current = max(c.current, r.version)
	}
	latest := m.latest()
	inBuild := map[int64]bool{}
	for _, f := range m.files {
		inBuild[f.version] = true
		r, ok := byVersion[f.version]
		switch {
		case !ok && f.version < c.current && !m.outOfOrder:
			c.drift = append(c.drift, fmt.Sprintf("%s is older than applied %s, so it would apply out of order (allow it with AllowOutOfOrder)", f.name, nameOf(byVersion[c.current])))
		case !ok:
			c.pending = append(c.pending, f)
		case r.checksum != f.checksum:
			c.drift = append(c.drift, fmt.Sprintf("%s was edited after it was applied (applied checksum %s, file %s)", f.name, short(r.checksum), short(f.checksum)))
		}
	}
	for _, r := range recs {
		if inBuild[r.version] {
			continue
		}
		if r.version > latest {
			c.ahead = append(c.ahead, r.name)
		} else {
			c.drift = append(c.drift, fmt.Sprintf("%s is applied, but this build doesn't have it", r.name))
		}
	}
	return c
}

func nameOf(r record) string { return strings.TrimSuffix(r.name, ".sql") }

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

func (m *Migrator) latest() int64 {
	if len(m.files) == 0 {
		return 0
	}
	return m.files[len(m.files)-1].version
}

func version(v int64) string {
	if v == 0 {
		return "none"
	}
	return strconv.FormatInt(v, 10)
}

func (m *Migrator) status(c compare) migrate.Status {
	s := migrate.Status{Expected: version(m.latest()), Current: version(c.current)}
	switch {
	case len(c.drift) > 0:
		s.State, s.Detail = migrate.Drifted, strings.Join(c.drift, "; ")
	case len(c.ahead) > 0:
		s.State, s.Detail = migrate.Ahead, "applied by a newer build: "+strings.Join(c.ahead, ", ")
	case len(c.pending) > 0:
		names := make([]string, len(c.pending))
		for i, f := range c.pending {
			names[i] = f.name
		}
		s.State, s.Detail = migrate.Pending, fmt.Sprintf("%d pending: %s", len(c.pending), strings.Join(names, ", "))
	default:
		s.State = migrate.UpToDate
	}
	return s
}

// Status compares the recorded versions with the files.
func (m *Migrator) Status(ctx context.Context) (migrate.Status, error) {
	recs, err := m.applied(ctx, m.db)
	if err != nil {
		return migrate.Status{}, fmt.Errorf("sqlmigrate: reading %s: %w", m.table.Sanitize(), err)
	}
	return m.status(m.compare(recs)), nil
}

// Verify lists every drifted file and every version beyond this build.
func (m *Migrator) Verify(ctx context.Context) (migrate.Drift, error) {
	recs, err := m.applied(ctx, m.db)
	if err != nil {
		return migrate.Drift{}, fmt.Errorf("sqlmigrate: reading %s: %w", m.table.Sanitize(), err)
	}
	c := m.compare(recs)
	var d migrate.Drift
	d.Differences = append(d.Differences, c.drift...)
	for _, a := range c.ahead {
		d.Differences = append(d.Differences, a+" is applied by a newer build")
	}
	return d, nil
}

// Plan lists the pending files, with their declared risks. The fingerprint
// covers their versions, names, risks and content.
func (m *Migrator) Plan(ctx context.Context) (migrate.Plan, error) {
	recs, err := m.applied(ctx, m.db)
	if err != nil {
		return nil, fmt.Errorf("sqlmigrate: reading %s: %w", m.table.Sanitize(), err)
	}
	return planOf(m.compare(recs).pending), nil
}

// PlanOffline plans every file, as if the database were empty, without
// connecting: what CI reviews. Its fingerprint isn't the online plan's,
// which covers only the pending files; approve the online one.
func (m *Migrator) PlanOffline(context.Context) (migrate.Plan, error) {
	return planOf(m.files), nil
}

func planOf(files []file) migrate.Plan {
	steps := make([]migrate.StepInfo, len(files))
	content := make([][]byte, len(files))
	for i, f := range files {
		steps[i], content[i] = f.step(), []byte(f.checksum)
	}
	return migrate.NewPlan(migrate.Fingerprint(steps, content), steps)
}

// Apply applies every pending file in order, each in its own transaction
// with its record and the fencing check. It refuses, before applying
// anything, a plan other than opts.Fingerprint or a file the options don't
// allow, and a database that is Drifted or Ahead.
func (m *Migrator) Apply(ctx context.Context, opts migrate.ApplyOptions) (migrate.Result, error) {
	if opts.Lock == nil {
		return migrate.Result{}, errs.New(errs.FailedPrecondition, CodeNoLock, fmt.Sprintf("sqlmigrate: migrator %q never applies without a lease", m.name))
	}
	// Everything that can refuse does so before anything is created.
	recs, err := m.applied(ctx, m.db)
	if err != nil {
		return migrate.Result{}, fmt.Errorf("sqlmigrate: reading %s: %w", m.table.Sanitize(), err)
	}
	c := m.compare(recs)
	if st := m.status(c); st.State == migrate.Drifted || st.State == migrate.Ahead {
		return migrate.Result{}, errs.New(errs.FailedPrecondition, CodeNotPending, fmt.Sprintf("sqlmigrate: migrator %q is %s, so there's nothing it may apply: %s", m.name, st.State, st.Detail))
	}
	pl := planOf(c.pending)
	if opts.Fingerprint != "" && opts.Fingerprint != pl.Fingerprint() {
		return migrate.Result{}, errs.New(errs.FailedPrecondition, migrate.CodeFingerprintMismatch, fmt.Sprintf("sqlmigrate: migrator %q: the pending plan's fingerprint is %s, not %s as approved; nothing was applied", m.name, pl.Fingerprint(), opts.Fingerprint))
	}
	for _, f := range c.pending {
		if !opts.Allows(f.risk) {
			return migrate.Result{}, errs.New(errs.FailedPrecondition, migrate.CodeRiskRefused, fmt.Sprintf("sqlmigrate: migrator %q: %s is of %s risk, which isn't allowed (max %s, unknown %s); nothing was applied", m.name, f.name, f.risk, migrate.Ceiling(opts.MaxRisk), allowed(opts.AllowUnknown)))
		}
	}
	if len(c.pending) == 0 {
		return migrate.Result{}, nil
	}
	if err := m.ensureTables(ctx); err != nil {
		return migrate.Result{}, err
	}
	var res migrate.Result
	fence := fencing{epoch: opts.Lock.Epoch(), token: int64(opts.Lock.Token())}
	for _, f := range c.pending {
		sctx, done := opts.Step(ctx, f.step())
		applied, err := m.applyFile(sctx, f, fence)
		done(err)
		if err != nil {
			return res, err
		}
		if applied {
			res.Applied = append(res.Applied, f.step())
		}
	}
	return res, nil
}

func allowed(b bool) string {
	if b {
		return "allowed"
	}
	return "refused"
}

// fencing is the lease a file is applied under.
type fencing struct {
	epoch string
	token int64
}

func (m *Migrator) fence() pgx.Identifier {
	t := slices.Clone(m.table)
	t[len(t)-1] += fenceSuffix
	return t
}

// ensureTables creates the tables, under a transaction-scoped advisory
// lock so that migrators sharing them don't race creating them.
func (m *Migrator) ensureTables(ctx context.Context) error {
	err := pgx.BeginFunc(ctx, m.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", m.table.Sanitize()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+m.table.Sanitize()+` (
	migrator   text        NOT NULL,
	version    bigint      NOT NULL,
	name       text        NOT NULL,
	checksum   text        NOT NULL,
	token      bigint      NOT NULL,
	applied_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (migrator, version)
);
CREATE TABLE IF NOT EXISTS `+m.fence().Sanitize()+` (
	migrator   text   PRIMARY KEY,
	epoch      text   NOT NULL,
	token      bigint NOT NULL,
	superseded text[] NOT NULL DEFAULT '{}'
)`)
		if err != nil {
			return err
		}
		// A fencing table made before epochs, or before superseded epochs
		// were kept, gains the columns; its rows get the empty epoch, which
		// the next lease supersedes. The table is altered only then: ALTER
		// waits for every lock on it, such as another migrator's file that
		// holds its fencing row.
		var have int
		err = tx.QueryRow(ctx, `SELECT count(*) FROM pg_attribute WHERE attrelid = $1::regclass AND attname IN ('epoch', 'superseded') AND NOT attisdropped`, m.fence().Sanitize()).Scan(&have)
		if err != nil || have == 2 {
			return err
		}
		_, err = tx.Exec(ctx, `ALTER TABLE `+m.fence().Sanitize()+`
	ADD COLUMN IF NOT EXISTS epoch      text   NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS superseded text[] NOT NULL DEFAULT '{}'`)
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlmigrate: creating %s: %w", m.table.Sanitize(), err)
	}
	return nil
}

// raiseFence moves the fencing row to the lease's epoch and token, or
// fails if the lease is stale: the row holds its epoch with a higher token,
// or its epoch has been superseded. A lease of a new epoch takes the row
// over, since its tokens come from another counter, and the epoch it
// replaces is kept as superseded, so a holder of that epoch can never take
// the row back. The row stays locked until tx ends, so the files of two
// holders never interleave.
func (m *Migrator) raiseFence(ctx context.Context, tx pgx.Tx, l fencing) error {
	table := m.fence().Sanitize()
	tag, err := tx.Exec(ctx, `INSERT INTO `+table+` (migrator, epoch, token) VALUES ($1, $2, $3) ON CONFLICT (migrator) DO NOTHING`, m.name, l.epoch, l.token)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var (
		epoch      string
		token      int64
		superseded []string
	)
	err = tx.QueryRow(ctx, `SELECT epoch, token, superseded FROM `+table+` WHERE migrator = $1 FOR UPDATE`, m.name).Scan(&epoch, &token, &superseded)
	if err != nil {
		return err
	}
	switch {
	case epoch == l.epoch && token > l.token:
		return errs.New(errs.FailedPrecondition, CodeStaleToken, fmt.Sprintf("sqlmigrate: migrator %q: the lease's fencing token %d is stale: a holder of token %d in its epoch has applied since", m.name, l.token, token))
	case epoch == l.epoch:
		_, err = tx.Exec(ctx, `UPDATE `+table+` SET token = $2 WHERE migrator = $1`, m.name, l.token)
	case slices.Contains(superseded, l.epoch):
		return errs.New(errs.FailedPrecondition, CodeStaleEpoch, fmt.Sprintf("sqlmigrate: migrator %q: the lease's epoch %q is stale: a holder of epoch %q has applied since", m.name, l.epoch, epoch))
	default:
		_, err = tx.Exec(ctx, `UPDATE `+table+` SET epoch = $2, token = $3, superseded = array_append(superseded, epoch) WHERE migrator = $1`, m.name, l.epoch, l.token)
	}
	return err
}

// applyFile applies f, unless it has been recorded since the status was
// read, and reports whether it did.
func (m *Migrator) applyFile(ctx context.Context, f file, fence fencing) (bool, error) {
	wrap := func(err error) error {
		if e := (*errs.Error)(nil); errors.As(err, &e) {
			return err
		}
		return fmt.Errorf("sqlmigrate: migrator %q: %s: %w", m.name, f.name, err)
	}
	var applied bool
	record := func(tx pgx.Tx) error {
		if err := m.raiseFence(ctx, tx, fence); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT EXISTS (SELECT 1 FROM "+m.table.Sanitize()+" WHERE migrator = $1 AND version = $2)", m.name, f.version)
		if err != nil {
			return err
		}
		done, err := pgx.CollectExactlyOneRow(rows, pgx.RowTo[bool])
		if err != nil || done {
			return err
		}
		if !f.noTx {
			if _, err := tx.Exec(ctx, f.sql); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, "INSERT INTO "+m.table.Sanitize()+" (migrator, version, name, checksum, token) VALUES ($1, $2, $3, $4, $5)", m.name, f.version, f.name, f.checksum, fence.token)
		applied = err == nil
		return err
	}
	if f.noTx {
		// Check the fence, and that the file is still pending, before
		// running it outside any transaction; the record then checks both
		// again.
		var pending bool
		err := pgx.BeginFunc(ctx, m.db, func(tx pgx.Tx) error {
			if err := m.raiseFence(ctx, tx, fence); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, "SELECT NOT EXISTS (SELECT 1 FROM "+m.table.Sanitize()+" WHERE migrator = $1 AND version = $2)", m.name, f.version)
			if err != nil {
				return err
			}
			pending, err = pgx.CollectExactlyOneRow(rows, pgx.RowTo[bool])
			return err
		})
		if err != nil || !pending {
			return false, wrapIf(err, wrap)
		}
		if _, err := m.db.Exec(ctx, f.sql); err != nil {
			return false, wrap(err)
		}
	}
	if err := pgx.BeginFunc(ctx, m.db, record); err != nil {
		return false, wrap(err)
	}
	return applied, nil
}

func wrapIf(err error, wrap func(error) error) error {
	if err == nil {
		return nil
	}
	return wrap(err)
}
