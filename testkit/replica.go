package testkit

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReplicaEnv names the environment variable holding the DSN of a streaming
// replica of the server PostgresEnv names, such as
// postgres://postgres@localhost:5433/postgres?sslmode=disable. Its user
// must be allowed to pause and resume replay (a superuser, or one granted
// pg_wal_replay_pause and pg_wal_replay_resume). When it is unset, tests
// that need a replica skip; in CI they fail instead.
const ReplicaEnv = "GALLEON_TEST_POSTGRES_REPLICA"

// Replica is a test's view of the replica: the package's database there,
// and replay to pause and resume. Pausing replay pauses it for everyone on
// the replica, so tests that use one take turns, across packages and
// processes, from Replica until they end.
type Replica struct {
	t     testing.TB
	dsn   string
	admin *pgx.Conn // on the replica
	turn  *pgx.Conn // on the primary, holding the turn
}

// replicaTurn is the advisory lock key, in SQL, of the turn on the replica.
const replicaTurn = "hashtext('galleon.testkit.replica')"

// Replica returns the replica's view of the package's database, waiting
// for its turn on the replica and then for it to have replayed what the
// primary had, or skips the test when ReplicaEnv is unset. Replay is
// resumed when the test ends, as it is when the turn is taken.
func (d *Database) Replica(t testing.TB) *Replica {
	t.Helper()
	env := strings.TrimSpace(os.Getenv(ReplicaEnv))
	if env == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI, so the tests that need a replica run", ReplicaEnv)
		}
		t.Skipf("%s is not set; skipping a test that needs a replica", ReplicaEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	turn, err := pgx.Connect(ctx, adminDSN())
	if err != nil {
		t.Fatalf("testkit.Replica: connecting to %s: %v", PostgresEnv, err)
	}
	if _, err := turn.Exec(ctx, "SELECT pg_advisory_lock("+replicaTurn+")"); err != nil {
		turn.Close(ctx)
		t.Fatalf("testkit.Replica: waiting for the turn on the replica: %v", err)
	}
	r := &Replica{t: t, turn: turn}
	t.Cleanup(r.close)
	if r.admin, err = pgx.Connect(ctx, env); err != nil {
		t.Fatalf("testkit.Replica: connecting to %s: %v", ReplicaEnv, err)
	}
	var standby bool
	if err := r.admin.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&standby); err != nil || !standby {
		t.Fatalf("testkit.Replica: %s must name a standby of %s (in recovery: %v, %v)", ReplicaEnv, PostgresEnv, standby, err)
	}
	cc, err := pgx.ParseConfig(d.dsn)
	if err == nil {
		r.dsn, err = withDatabase(env, cc.Database)
	}
	if err != nil {
		t.Fatalf("testkit.Replica: %v", err)
	}
	r.Resume()
	r.CatchUp()
	return r
}

// DSN is the package's database on the replica.
func (r *Replica) DSN() string { return r.dsn }

func (r *Replica) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if r.admin != nil {
		if _, err := r.admin.Exec(ctx, "SELECT pg_wal_replay_resume()"); err != nil {
			r.t.Errorf("testkit.Replica: resuming replay: %v", err)
		}
		r.admin.Close(ctx)
	}
	// Closing the session gives the turn back.
	r.turn.Close(ctx)
}

// Pause pauses replay on the replica, and returns once it has paused:
// from then on the replica lags behind every write the primary commits.
func (r *Replica) Pause() {
	r.t.Helper()
	r.exec("SELECT pg_wal_replay_pause()")
	r.until("replay to pause", func(ctx context.Context) (bool, error) {
		var state string
		err := r.admin.QueryRow(ctx, "SELECT pg_get_wal_replay_pause_state()").Scan(&state)
		return state == "paused", err
	})
}

// Resume resumes replay.
func (r *Replica) Resume() {
	r.t.Helper()
	r.exec("SELECT pg_wal_replay_resume()")
}

// CatchUp returns once the replica has replayed everything the primary had
// written when it was called. Replay must not be paused.
func (r *Replica) CatchUp() {
	r.t.Helper()
	var target string
	if err := r.turn.QueryRow(context.Background(), "SELECT pg_current_wal_lsn()::text").Scan(&target); err != nil {
		r.t.Fatalf("testkit.Replica: reading the primary's position: %v", err)
	}
	r.until("the replica to catch up", func(ctx context.Context) (bool, error) {
		var done bool
		err := r.admin.QueryRow(ctx, "SELECT coalesce(pg_last_wal_replay_lsn() >= $1::pg_lsn, false)", target).Scan(&done)
		return done, err
	})
}

func (r *Replica) exec(sql string) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.admin.Exec(ctx, sql); err != nil {
		r.t.Fatalf("testkit.Replica: %s: %v", sql, err)
	}
}

// until polls cond until it holds, for up to 30 seconds.
func (r *Replica) until(what string, cond func(context.Context) (bool, error)) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		ok, err := cond(ctx)
		if err != nil {
			r.t.Fatalf("testkit.Replica: waiting for %s: %v", what, err)
		}
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			r.t.Fatalf("testkit.Replica: %s didn't happen within 30s", what)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
