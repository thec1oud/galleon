package datatest_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/op"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/server"
	"github.com/thec1oud/galleon/testkit"
	"github.com/thec1oud/galleon/testkit/conformance/datatest"
)

// The tests here run against a real streaming replica, and skip without
// one (testkit.ReplicaEnv); CI provisions one.

// replicaEnv is a test's table, committed on the primary and replayed on
// the replica.
type replicaEnv struct {
	t     *testing.T
	db    *testkit.Database
	rep   *testkit.Replica
	pool  *pgxpool.Pool
	table string
}

func newReplicaEnv(t *testing.T) *replicaEnv {
	t.Helper()
	db := testkit.DB(t)
	rep := db.Replica(t)
	pool, err := pgxpool.New(context.Background(), db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	e := &replicaEnv{t: t, db: db, rep: rep, pool: pool, table: fmt.Sprintf("replica_%d", time.Now().UnixNano())}
	e.exec("CREATE TABLE " + e.table + " (id int PRIMARY KEY)")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+e.table) })
	rep.CatchUp()
	return e
}

func (e *replicaEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// manager starts a Manager on the primary and the replica (or replicaDSN),
// tracked every 10ms.
func (e *replicaEnv) manager(cfg pg.Config, opts ...pg.Option) *pg.Manager {
	e.t.Helper()
	if cfg.Primary.DSN == "" {
		cfg.Primary.DSN = e.db.DSN()
	}
	if cfg.Replicas == nil {
		cfg.Replicas = []pg.Source{{DSN: e.rep.DSN()}}
	}
	if cfg.LagPoll == 0 {
		cfg.LagPoll = 10 * time.Millisecond
	}
	m, err := pg.New(cfg, opts...)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m
}

// where reads row id at level l and says whether it was found, and on the
// replica.
func (e *replicaEnv) where(ctx context.Context, m data.Manager, id int, l data.Level) (found, onReplica bool, err error) {
	err = m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
		return r.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+e.table+" WHERE id = $1), pg_is_in_recovery()", id).Scan(&found, &onReplica)
	}, data.Consistency(l))
	return found, onReplica, err
}

// until polls cond until it holds, failing after 30 seconds.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 30s for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *replicaEnv) untilOnReplica(m data.Manager, want bool) {
	e.t.Helper()
	until(e.t, fmt.Sprintf("Any reads to run on the replica: %v", want), func() bool {
		_, on, err := e.where(context.Background(), m, 0, data.Any)
		if err != nil {
			e.t.Fatalf("a read failed: %v", err)
		}
		return on == want
	})
}

// syncBuffer is a log destination for a test to read.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestReplicaReadsAreReadOnlyAndWritesNeverReachAReplica(t *testing.T) {
	e := newReplicaEnv(t)
	logs := &syncBuffer{}
	m := e.manager(pg.Config{}, pg.Logger(slog.New(slog.NewTextHandler(logs, nil))))
	e.untilOnReplica(m, true)
	ctx := context.Background()

	err := m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
		rows, err := r.Query(ctx, "INSERT INTO "+e.table+" (id) VALUES (1)")
		if err == nil {
			rows.Close()
			err = rows.Err()
		}
		return err
	}, data.Consistency(data.Any))
	if err == nil {
		t.Fatal("a write through a replica's read transaction worked")
	}
	if _, err := m.BeginWrite(ctx, data.Source("replica_1")); errs.From(err).Code != data.CodeUnknownSource {
		t.Fatalf("a replica can be named as a source: %v", err)
	}
	if found, on, err := e.where(ctx, m, 0, data.Primary); err != nil || on {
		t.Fatalf("a Primary read ran on the replica: %v %v", found, err)
	}

	err = m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
		if _, err := w.Exec(ctx, "INSERT INTO "+e.table+" (id) VALUES (2)"); err != nil {
			return err
		}
		var inRecovery bool
		if err := w.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil || inRecovery {
			t.Errorf("a write ran on the replica: %v", err)
		}
		for _, p := range []data.Propagation{data.Join, data.Nested} {
			var found, on bool
			r, err := m.BeginRead(ctx, p, data.Consistency(data.Any))
			if err != nil {
				return err
			}
			err = r.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+e.table+" WHERE id = 2), pg_is_in_recovery()").Scan(&found, &on)
			_ = r.Commit(ctx)
			if err != nil || on || !found {
				t.Errorf("a %v read in a write transaction left it: found %v, on the replica %v, %v", p, found, on, err)
			}
		}
		// Independent may leave, and is flagged in development.
		r, err := m.BeginRead(ctx, data.Independent, data.Consistency(data.Any))
		if err != nil {
			return err
		}
		return r.Rollback(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "an Independent read began while a write transaction is open") {
		t.Fatalf("no warning for an Independent read in a write transaction:\n%s", logs)
	}
}

func TestALaggingReplicaLeavesRotationAndReturnsOnceCaughtUp(t *testing.T) {
	e := newReplicaEnv(t)
	logs := &syncBuffer{}
	tp := &datatest.Recorder{}
	m := e.manager(pg.Config{MaxLag: 200 * time.Millisecond}, pg.Logger(slog.New(slog.NewTextHandler(logs, nil))), pg.TracerProvider(tp))
	e.untilOnReplica(m, true)

	e.rep.Pause()
	// The replica lags once the primary moves on without it.
	e.exec("INSERT INTO " + e.table + " (id) VALUES (1)")
	e.untilOnReplica(m, false)
	if !strings.Contains(logs.String(), "replica left rotation") || !strings.Contains(logs.String(), "reason=lagging") {
		t.Errorf("leaving rotation wasn't logged:\n%s", logs)
	}
	fell := tp.Find(func(s *datatest.Span) bool {
		return s.Name() == "transaction" && s.Attr("galleon.data.fallback") == "no_replica" && s.Attr("galleon.data.consistency") == "any"
	})
	if len(fell) == 0 {
		t.Error("no span says a read fell back to the primary")
	}

	e.rep.Resume()
	e.untilOnReplica(m, true)
	if !strings.Contains(logs.String(), "replica joined rotation") {
		t.Errorf("rejoining wasn't logged:\n%s", logs)
	}
}

func TestAReplicaThatGoesDownIsLeftOutUntilItComesBack(t *testing.T) {
	e := newReplicaEnv(t)
	proxy := testkit.NewProxy(t, e.rep.DSN())
	m := e.manager(pg.Config{Replicas: []pg.Source{{DSN: proxy.DSN(e.rep.DSN())}}})
	e.untilOnReplica(m, true)

	proxy.Cut()
	// Every read works while the replica is down, from the primary.
	e.untilOnReplica(m, false)
	for range 20 {
		if _, on, err := e.where(context.Background(), m, 0, data.ReadYourWrites.Wait(time.Second)); err != nil || on {
			t.Fatalf("with the replica down: on the replica %v, %v", on, err)
		}
	}
	proxy.Restore()
	e.untilOnReplica(m, true)

	// A replica down from the start doesn't stop the Manager starting.
	proxy.Cut()
	m2 := e.manager(pg.Config{Replicas: []pg.Source{{DSN: proxy.DSN(e.rep.DSN())}}})
	if _, on, err := e.where(context.Background(), m2, 0, data.Any); err != nil || on {
		t.Fatalf("all replicas down: on the replica %v, %v", on, err)
	}
	proxy.Restore()
	e.untilOnReplica(m2, true)
}

func TestAPrimaryThatIsDownIsUnavailableToPrimaryReadsAndWrites(t *testing.T) {
	e := newReplicaEnv(t)
	proxy := testkit.NewProxy(t, e.db.DSN())
	m := e.manager(pg.Config{Primary: pg.Source{DSN: proxy.DSN(e.db.DSN())}})
	e.untilOnReplica(m, true)
	proxy.Cut()
	ctx := context.Background()
	if _, _, err := e.where(ctx, m, 0, data.Primary); errs.KindOf(err) != errs.Unavailable {
		t.Fatalf("a Primary read: %v", err)
	}
	if err := m.Write(ctx, func(context.Context, data.WriteTx) error { return nil }); errs.KindOf(err) != errs.Unavailable {
		t.Fatalf("a write: %v", err)
	}
}

// When a database comes back, the connections the pool kept from before
// are dead; the first transaction after replaces them instead of failing.
func TestTheFirstTransactionAfterADatabaseReturnsSucceeds(t *testing.T) {
	db := testkit.DB(t)
	proxy := testkit.NewProxy(t, db.DSN())
	m, err := pg.New(pg.Config{Primary: pg.Source{DSN: proxy.DSN(db.DSN()), MaxConns: 4}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	// Fill the pool: four transactions at once.
	var held []data.ReadTx
	for range 4 {
		r, err := m.BeginRead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var x int
		if err := r.QueryRow(ctx, "SELECT 1").Scan(&x); err != nil {
			t.Fatal(err)
		}
		held = append(held, r)
	}
	for _, r := range held {
		if err := r.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	proxy.Cut()
	proxy.Restore()
	for _, write := range []bool{false, true} {
		var err error
		if write {
			err = m.Write(ctx, func(context.Context, data.WriteTx) error { return nil }, data.Retry(data.NoRetry))
		} else {
			err = m.Read(ctx, func(context.Context, data.ReadTx) error { return nil }, data.Retry(data.NoRetry))
		}
		if err != nil {
			t.Fatalf("the first transaction (write %v) after the database came back: %v", write, err)
		}
	}
}

// The commit's WAL position is read in the round trip of the COMMIT
// itself: what the client sends to commit is one simple query.
func TestTheCommitPositionComesWithTheCommitInOneRoundTrip(t *testing.T) {
	db := testkit.DB(t)
	proxy := testkit.NewProxy(t, db.DSN())
	m, err := pg.New(pg.Config{Primary: pg.Source{DSN: proxy.DSN(db.DSN()), MaxConns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := pg.CausalContext(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	table := fmt.Sprintf("commit_%d", time.Now().UnixNano())
	if err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
		_, err := w.Exec(ctx, "CREATE TABLE "+table+" (id int)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.Write(context.Background(), func(ctx context.Context, w data.WriteTx) error {
			_, err := w.Exec(ctx, "DROP TABLE "+table)
			return err
		})
	})

	w, err := m.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(ctx, "INSERT INTO "+table+" VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	proxy.Reset()
	if err := w.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	msgs := proxy.Messages()
	if len(msgs) != 1 || msgs[0].Type != 'Q' || !strings.Contains(msgs[0].SQL, "COMMIT") || !strings.Contains(msgs[0].SQL, "pg_current_wal_insert_lsn()") {
		t.Fatalf("committing sent %+v, want one simple query that commits and reads the position", msgs)
	}

	// A read commits as it always did.
	r, err := m.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proxy.Reset()
	if err := r.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if msgs := proxy.Messages(); len(msgs) != 1 || msgs[0].SQL != "commit" {
		t.Fatalf("a read's commit sent %+v", msgs)
	}

	// A failed transaction's commit rolls back, as before.
	w, err = m.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Exec(ctx, "SELECT 1/0")
	if err := w.Commit(ctx); err == nil {
		t.Fatal("a failed transaction committed")
	}
	if err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
		var n int
		return w.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)
	}); err != nil {
		t.Fatalf("the connection is unusable after: %v", err)
	}
}

func TestTheTrackerStopsWithTheManagerAndStartsAgain(t *testing.T) {
	e := newReplicaEnv(t)
	pollers := func() int {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		n := 0
		for _, g := range strings.Split(string(buf), "\n\n") {
			// One in WaitGroup.Done has done its work and is returning: Stop
			// has been released by that Done.
			if strings.Contains(g, "galleon/pg.(*topology).") && !strings.Contains(g, "sync.(*WaitGroup).Done") {
				n++
			}
		}
		return n
	}
	m, err := pg.New(pg.Config{Primary: pg.Source{DSN: e.db.DSN()}, Replicas: []pg.Source{{DSN: e.rep.DSN()}}, LagPoll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for round := range 2 {
		before := pollers()
		if err := m.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if n := pollers() - before; n != 2 {
			t.Fatalf("round %d: %d pollers, want one for the primary and one for the replica", round, n)
		}
		e.untilOnReplica(m, true)
		if err := m.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		// Stop waits for them.
		if n := pollers() - before; n != 0 {
			t.Fatalf("round %d: %d pollers outlived Stop", round, n)
		}
	}
}

// recMeter keeps what the data plane counts and observes.
type recMeter struct {
	noop.Meter
	mu        sync.Mutex
	counts    map[string]int64
	callbacks []metric.Callback
}

type recCounter struct {
	noop.Int64Counter
	m    *recMeter
	name string
}

func (c recCounter) Add(_ context.Context, n int64, opts ...metric.AddOption) {
	cfg := metric.NewAddConfig(opts)
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	key := c.name
	set := cfg.Attributes()
	for _, kv := range set.ToSlice() {
		key += " " + string(kv.Key) + "=" + kv.Value.Emit()
	}
	c.m.counts[key] += n
}

type (
	recIntGauge struct {
		noop.Int64ObservableGauge
		name string
	}
	recFloatGauge struct {
		noop.Float64ObservableGauge
		name string
	}
)

func (m *recMeter) Int64Counter(name string, _ ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return recCounter{m: m, name: name}, nil
}
func (m *recMeter) Int64ObservableGauge(name string, _ ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	return recIntGauge{name: name}, nil
}
func (m *recMeter) Float64ObservableGauge(name string, _ ...metric.Float64ObservableGaugeOption) (metric.Float64ObservableGauge, error) {
	return recFloatGauge{name: name}, nil
}
func (m *recMeter) RegisterCallback(f metric.Callback, _ ...metric.Observable) (metric.Registration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callbacks = append(m.callbacks, f)
	return noop.Registration{}, nil
}

// observe runs the callbacks and returns what they observed, by
// instrument and attributes.
func (m *recMeter) observe() map[string]float64 {
	m.mu.Lock()
	cbs := append([]metric.Callback(nil), m.callbacks...)
	m.mu.Unlock()
	o := &recObserver{seen: map[string]float64{}}
	for _, f := range cbs {
		_ = f(context.Background(), o)
	}
	return o.seen
}

func (m *recMeter) count(prefix string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for k, v := range m.counts {
		if strings.HasPrefix(k, prefix) {
			n += v
		}
	}
	return n
}

type recObserver struct {
	embedded.Observer
	seen map[string]float64
}

func attrsOf(opts []metric.ObserveOption) string {
	var s string
	set := metric.NewObserveConfig(opts).Attributes()
	for _, kv := range set.ToSlice() {
		s += " " + string(kv.Key) + "=" + kv.Value.Emit()
	}
	return s
}

func (o *recObserver) ObserveFloat64(i metric.Float64Observable, v float64, opts ...metric.ObserveOption) {
	o.seen[i.(recFloatGauge).name+attrsOf(opts)] = v
}
func (o *recObserver) ObserveInt64(i metric.Int64Observable, v int64, opts ...metric.ObserveOption) {
	o.seen[i.(recIntGauge).name+attrsOf(opts)] = float64(v)
}

type recProvider struct {
	embedded.MeterProvider
	m *recMeter
}

func (p recProvider) Meter(string, ...metric.MeterOption) metric.Meter { return p.m }

func TestSpansAndMetricsNameTheReplicaAndTheFallbacks(t *testing.T) {
	e := newReplicaEnv(t)
	tp := &datatest.Recorder{}
	meter := &recMeter{counts: map[string]int64{}}
	m := e.manager(pg.Config{MaxLag: time.Hour}, pg.TracerProvider(tp), pg.MeterProvider(recProvider{m: meter}))
	e.untilOnReplica(m, true)

	onReplica := tp.Find(func(s *datatest.Span) bool {
		return s.Name() == "transaction" && s.Attr("galleon.data.replica") == "replica_1" && s.Attr("galleon.data.consistency") == "any"
	})
	if len(onReplica) == 0 {
		t.Fatalf("no transaction span names the replica: %v", tp.Names())
	}
	stmts := tp.Find(func(s *datatest.Span) bool { return s.Parent() == onReplica[0] && s.Attr("db.query.text") != "" })
	if len(stmts) == 0 || stmts[0].Attr("galleon.data.replica") != "replica_1" || stmts[0].Attr("galleon.data.source") != "primary" {
		t.Errorf("a statement on the replica doesn't say so: %v", stmts)
	}
	seen := meter.observe()
	if seen["galleon.data.replica.in_rotation galleon.data.replica=replica_1"] != 1 {
		t.Errorf("rotation isn't observed: %v", seen)
	}
	if _, ok := seen["galleon.data.replica.lag galleon.data.replica=replica_1"]; !ok {
		t.Errorf("lag isn't observed: %v", seen)
	}
	// Lag in bytes is known, and observed, once the tracker has a reading
	// of the primary, which a replica in rotation by its last replayed
	// commit alone may not have waited for.
	until(t, "lag in bytes to be observed", func() bool {
		_, ok := meter.observe()["galleon.data.replica.lag_bytes galleon.data.replica=replica_1"]
		return ok
	})
	if meter.count("galleon.data.reads galleon.data.consistency=any galleon.data.target=replica_1") == 0 {
		t.Errorf("reads on the replica aren't counted: %v", meter.counts)
	}

	// A ReadYourWrites read after a write the replica hasn't replayed
	// waits, then reads the primary.
	e.rep.Pause()
	ctx := pg.CausalContext(context.Background())
	if err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
		_, err := w.Exec(ctx, "INSERT INTO "+e.table+" (id) VALUES (1)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if found, on, err := e.where(ctx, m, 1, data.ReadYourWrites.Wait(30*time.Millisecond)); err != nil || !found || on {
		t.Fatalf("found %v, on the replica %v, %v", found, on, err)
	}
	waited := tp.Find(func(s *datatest.Span) bool {
		return s.Name() == "transaction" && s.Attr("galleon.data.fallback") == "wait_timeout" && s.Attr("galleon.data.wait.duration") != ""
	})
	if len(waited) != 1 {
		t.Errorf("no span says the read waited and fell back")
	}
	if meter.count("galleon.data.read.fallbacks galleon.data.consistency=read_your_writes galleon.data.fallback=wait_timeout") != 1 {
		t.Errorf("the fallback isn't counted: %v", meter.counts)
	}
	for _, s := range tp.Find(func(*datatest.Span) bool { return true }) {
		if s.Contains("postgres://") || s.Contains("sslmode") {
			t.Fatalf("span %s holds a DSN", s.Name())
		}
	}
}

// Notes is an app with operations over HTTP and RPC, for tokens across
// requests and apps.
type NoteIn struct {
	ID int `json:"id"`
}

type NoteOut struct {
	Found     bool `json:"found"`
	OnReplica bool `json:"on_replica"`
}

func notesApp(t *testing.T, e *replicaEnv, key string) http.Handler {
	t.Helper()
	return notesAppOn(t, e, key, e.db.DSN())
}

// notesAppOn is notesApp with its primary at dsn.
func notesAppOn(t *testing.T, e *replicaEnv, key, dsn string) http.Handler {
	t.Helper()
	var m data.Manager
	mod := galleon.ModuleFunc("notes", func(r *galleon.Registry) error {
		r.Operation(op.Unary[NoteIn, NoteOut]("notes.add").HTTP("POST", "/notes/add").RPC("notes.v1.Notes", "Add").Public(), func(ctx context.Context, in *NoteIn) (*NoteOut, error) {
			return &NoteOut{}, m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
				_, err := w.Exec(ctx, "INSERT INTO "+e.table+" (id) VALUES ($1)", in.ID)
				return err
			})
		})
		r.Operation(op.Unary[NoteIn, NoteOut]("notes.get").HTTP("POST", "/notes/get").RPC("notes.v1.Notes", "Get").Public().Consistency(data.ReadYourWrites), func(ctx context.Context, in *NoteIn) (*NoteOut, error) {
			var out NoteOut
			found, on, err := e.where(ctx, m, in.ID, data.ReadYourWrites)
			out.Found, out.OnReplica = found, on
			return &out, err
		})
		return nil
	})
	cfg, _ := json.Marshal(map[string]any{
		"http": map[string]any{"addr": "127.0.0.1:0"},
		"pg": map[string]any{
			"primary":   map[string]any{"dsn": dsn},
			"replicas":  []any{map[string]any{"dsn": e.rep.DSN()}},
			"lag_poll":  int64(10 * time.Millisecond),
			"max_lag":   int64(time.Hour),
			"token_key": key,
		},
	})
	app, err := galleon.New(galleon.Name("notes"), galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode), galleon.DrainGrace(0),
		galleon.ConfigJSON(cfg), galleon.Modules(pg.Module(), server.Module(), mod))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	m = galleon.MustResolve[data.Manager](app)
	return galleon.MustResolve[*server.Server](app).Handler()
}

func post(t *testing.T, h http.Handler, path string, id int, hdr http.Header) (NoteOut, http.Header) {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(fmt.Sprintf(`{"id":%d}`, id)))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out NoteOut
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body)
	}
	return out, w.Header()
}

type jsonCodec struct{}

func (jsonCodec) Name() string                    { return "json" }
func (jsonCodec) Marshal(v any) ([]byte, error)   { return json.Marshal(v) }
func (jsonCodec) Unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func rpc(t *testing.T, srv *httptest.Server, method string, id int, hdr http.Header) (NoteOut, http.Header) {
	t.Helper()
	c := connect.NewClient[NoteIn, NoteOut](srv.Client(), srv.URL+"/notes.v1.Notes/"+method, connect.WithCodec(jsonCodec{}))
	req := connect.NewRequest(&NoteIn{ID: id})
	for k, v := range hdr {
		req.Header()[k] = v
	}
	res, err := c.CallUnary(context.Background(), req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return *res.Msg, res.Header()
}

func TestTokensCarryReadYourWritesAcrossRequestsAndApps(t *testing.T) {
	e := newReplicaEnv(t)
	const key = "a key the two apps share, of 32 bytes or more"
	a, b := notesApp(t, e, key), notesApp(t, e, key)
	stranger := notesApp(t, e, "a key of another deployment, also 32 bytes long")
	for _, h := range []http.Handler{a, b} {
		until(t, "reads to run on the replica", func() bool {
			out, _ := post(t, h, "/notes/get", 0, nil)
			return out.OnReplica
		})
	}
	e.rep.Pause()

	_, hdr := post(t, a, "/notes/add", 1, nil)
	tok := hdr.Get(pg.ConsistencyHeader)
	if tok == "" {
		t.Fatal("a write over HTTP sent no token")
	}
	if _, hdr := post(t, a, "/notes/get", 1, nil); hdr.Get(pg.ConsistencyHeader) != "" {
		t.Fatal("a read sent a token")
	}
	with := http.Header{pg.ConsistencyHeader: {tok}}
	if out, _ := post(t, b, "/notes/get", 1, nil); out.Found || !out.OnReplica {
		t.Fatalf("without the token the lagging replica serves, and hasn't the write: %+v", out)
	}
	if out, _ := post(t, b, "/notes/get", 1, with); !out.Found {
		t.Fatalf("another app, with the token: %+v", out)
	}
	if out, _ := post(t, stranger, "/notes/get", 1, with); out.Found {
		t.Fatalf("an app with another key honored the token: %+v", out)
	}

	sa, sb := httptest.NewServer(a), httptest.NewServer(b)
	t.Cleanup(sa.Close)
	t.Cleanup(sb.Close)
	_, rh := rpc(t, sa, "Add", 2, nil)
	rtok := rh.Get(pg.ConsistencyHeader)
	if rtok == "" {
		t.Fatal("a write over RPC sent no token")
	}
	if out, _ := rpc(t, sb, "Get", 2, nil); out.Found {
		t.Fatalf("over RPC, without the token: %+v", out)
	}
	if out, _ := rpc(t, sb, "Get", 2, http.Header{pg.ConsistencyHeader: {rtok}}); !out.Found {
		t.Fatalf("over RPC, with the token: %+v", out)
	}
}

// asRole creates a login role without superuser for the test, and returns
// dsn for it.
func asRole(t *testing.T, pool *pgxpool.Pool, dsn string) (string, string) {
	t.Helper()
	role := fmt.Sprintf("galleon_role_%d", time.Now().UnixNano())
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE ROLE "+role+" LOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP OWNED BY "+role)
		_, _ = pool.Exec(context.Background(), "DROP ROLE "+role)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.User(role)
	return u.String(), role
}

// revoke takes fn, a function of the package's database, from everyone but
// superusers until the test ends.
func revoke(t *testing.T, pool *pgxpool.Pool, fn string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "REVOKE EXECUTE ON FUNCTION "+fn+" FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "GRANT EXECUTE ON FUNCTION "+fn+" TO PUBLIC"); err != nil {
			t.Errorf("restoring %s: %v", fn, err)
		}
	})
}

// A write whose COMMIT succeeded is committed, even when reading its
// position after fails: failing it would have a committed write retried.
func TestAWriteThatCommittedSucceedsWhenItsPositionCantBeRead(t *testing.T) {
	db := testkit.DB(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dsn, role := asRole(t, pool, db.DSN())
	table := fmt.Sprintf("committed_%d", time.Now().UnixNano())
	for _, sql := range []string{"CREATE TABLE " + table + " (id int PRIMARY KEY)", "GRANT ALL ON TABLE " + table + " TO " + role} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	revoke(t, pool, "pg_current_wal_insert_lsn()")

	m, err := pg.New(pg.Config{Primary: pg.Source{DSN: dsn}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	cctx := pg.CausalContext(ctx)
	runs := 0
	err = m.Write(cctx, func(ctx context.Context, w data.WriteTx) error {
		runs++
		_, err := w.Exec(ctx, "INSERT INTO "+table+" VALUES (1)")
		return err
	})
	if err != nil || runs != 1 {
		t.Fatalf("a committed write: %v, after %d runs", err, runs)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d rows, %v", n, err)
	}
	// The connection is still good for the next transaction.
	if err := m.Read(cctx, func(ctx context.Context, r data.ReadTx) error {
		return r.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
}

// A write whose position can't be read still sends a token. The next
// request with it reads the primary when it can't ask the primary for its
// position either, and otherwise a replica past the primary's position
// then.
func TestAWriteWhosePositionCantBeReadStillCarriesReadYourWrites(t *testing.T) {
	e := newReplicaEnv(t)
	dsn, role := asRole(t, e.pool, e.db.DSN())
	e.exec("GRANT ALL ON TABLE " + e.table + " TO " + role)
	const key = "a key the two apps share, of 32 bytes or more"
	blind, sighted := notesAppOn(t, e, key, dsn), notesApp(t, e, key)
	for _, h := range []http.Handler{blind, sighted} {
		until(t, "reads to run on the replica", func() bool {
			out, _ := post(t, h, "/notes/get", 0, nil)
			return out.OnReplica
		})
	}
	revoke(t, e.pool, "pg_current_wal_insert_lsn()")
	e.rep.Pause()

	_, hdr := post(t, blind, "/notes/add", 1, nil)
	tok := hdr.Get(pg.ConsistencyHeader)
	if tok == "" {
		t.Fatal("a write that committed, of unknown position, sent no token")
	}
	with := http.Header{pg.ConsistencyHeader: {tok}}
	if out, rh := post(t, blind, "/notes/get", 1, with); !out.Found || out.OnReplica || rh.Get(pg.ConsistencyHeader) != "" {
		t.Fatalf("an app that can't ask the primary its position: %+v, token %q", out, rh.Get(pg.ConsistencyHeader))
	}
	out, rh := post(t, sighted, "/notes/get", 1, with)
	if !out.Found || out.OnReplica {
		t.Fatalf("an app that can, with the replica paused: %+v", out)
	}
	// Having clamped it, the app sends the clamped floor in its place.
	fresh := rh.Get(pg.ConsistencyHeader)
	if lsn := tokenLSN(t, fresh); lsn == 0 || lsn == math.MaxUint64 {
		t.Fatalf("the clamped token was reissued at %d", lsn)
	}
	if out, _ := post(t, sighted, "/notes/get", 1, http.Header{pg.ConsistencyHeader: {fresh}}); !out.Found {
		t.Fatalf("with the reissued token: %+v", out)
	}
	e.rep.Resume()
	// The insert position the token is clamped to can sit past what has
	// been flushed, so replicated, on a quiet primary, by a record no
	// commit has flushed yet: a commit flushes it.
	e.exec("INSERT INTO " + e.table + " (id) VALUES (2)")
	until(t, "the token's reads to run on the caught-up replica", func() bool {
		out, _ := post(t, sighted, "/notes/get", 1, with)
		if !out.Found {
			t.Fatalf("a read with the token missed the write: %+v", out)
		}
		return out.OnReplica
	})
}

// tokenLSN is the position a causal token carries, for tests: the token
// is opaque to clients.
func tokenLSN(t *testing.T, tok string) uint64 {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(b) < 9 {
		t.Fatalf("token %q: %v", tok, err)
	}
	return binary.BigEndian.Uint64(b[1:9])
}

func TestStartWarnsWhenMaxLagIsBelowTheStalenessBound(t *testing.T) {
	e := newReplicaEnv(t)
	for _, c := range []struct {
		maxLag time.Duration
		warns  bool
	}{{100 * time.Millisecond, true}, {time.Second, false}} {
		logs := &syncBuffer{}
		e.manager(pg.Config{MaxLag: c.maxLag}, pg.Logger(slog.New(slog.NewTextHandler(logs, nil))))
		if got := strings.Contains(logs.String(), "max_lag is below the tracker's staleness bound"); got != c.warns {
			t.Errorf("MaxLag %v: warned %v:\n%s", c.maxLag, got, logs)
		}
	}
}

// When the tracker can't read the primary's position, though the pools
// work, a replica that lags isn't taken for caught up.
func TestAReplicaLagsWhenTheTrackerLosesThePrimary(t *testing.T) {
	e := newReplicaEnv(t)
	dsn, role := asRole(t, e.pool, e.db.DSN())
	e.exec("GRANT ALL ON TABLE " + e.table + " TO " + role)
	e.exec("INSERT INTO " + e.table + " (id) VALUES (100)")
	e.rep.CatchUp()
	repDSN, err := url.Parse(e.rep.DSN())
	if err != nil {
		t.Fatal(err)
	}
	repDSN.User = url.User(role)
	m := e.manager(pg.Config{Primary: pg.Source{DSN: dsn}, Replicas: []pg.Source{{DSN: repDSN.String()}}, MaxLag: time.Second})
	e.untilOnReplica(m, true)

	// From now on the primary's poller fails, and the replica falls
	// behind.
	revoke(t, e.pool, "pg_current_wal_lsn()")
	e.rep.CatchUp()
	e.rep.Pause()
	e.exec("INSERT INTO " + e.table + " (id) VALUES (1)")
	e.untilOnReplica(m, false)
	if found, _, err := e.where(context.Background(), m, 1, data.BoundedStaleness(time.Hour)); err != nil || !found {
		t.Fatalf("a bounded read found %v, %v", found, err)
	}
}

// A write whose commit ends exactly at a WAL page boundary leaves the
// insert position past the next page's header, where no replica stops: on
// an idle primary, its ReadYourWrites read still finds a replica.
func TestAWriteEndingAtAWALPageIsReadOnTheReplica(t *testing.T) {
	e := newReplicaEnv(t)
	m := e.manager(pg.Config{MaxLag: time.Hour})
	e.untilOnReplica(m, true)
	ctx := context.Background()
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// The layout the Manager reads, from the same query. The padding below
	// sizes WAL records for 8-byte alignment, which every 64-bit build has.
	var block, seg, align int64
	if err := conn.QueryRow(ctx, "SELECT wal_block_size, bytes_per_wal_segment, max_data_alignment FROM pg_control_init()").Scan(&block, &seg, &align); err != nil {
		t.Fatal(err)
	}
	if align != 8 {
		t.Skipf("the primary aligns WAL to %d bytes; this test can only force a page boundary at 8", align)
	}
	short, long := (20+align-1)/align*align, (36+align-1)/align*align
	insert := func() int64 {
		t.Helper()
		var lsn int64
		if err := conn.QueryRow(ctx, "SELECT (pg_current_wal_insert_lsn() - '0/0')::int8").Scan(&lsn); err != nil {
			t.Fatal(err)
		}
		return lsn
	}
	pad := func(n int64) {
		t.Helper()
		if _, err := conn.Exec(ctx, "SELECT pg_logical_emit_message(false, 'p', repeat('x', $1::int))", n); err != nil {
			t.Fatal(err)
		}
	}
	write := func(cctx context.Context, id int) {
		t.Helper()
		if err := m.Write(cctx, func(ctx context.Context, w data.WriteTx) error {
			_, err := w.Exec(ctx, "INSERT INTO "+e.table+" (id) VALUES ($1)", id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Other WAL, a checkpoint's full-page images among it, can land
	// between the padding and the write: each try measures the write, pads
	// up to where it will end at a page, and checks that it did.
	for try, id := 0, 1; try < 50; try++ {
		before := insert()
		write(pg.CausalContext(ctx), id)
		id++
		after := insert()
		size := after - before
		if after/block != before/block {
			// It crossed into a page, and its header.
			if after/seg != before/seg {
				size -= long
			} else {
				size -= short
			}
		}
		for range 20 {
			at := insert()
			gap := block - at%block
			if gap == size {
				break
			}
			// A message is its content and 52 bytes, aligned to 8, so 56
			// or more; 55 once its data passes 255 bytes and takes a long
			// length. Short of room, pad into the next page.
			n := gap - size - 52
			if n > 229 {
				n = gap - size - 55
			}
			if n < 4 {
				n = 200
			}
			pad(n)
		}
		at := insert()
		if block-at%block != size {
			continue
		}
		cctx := pg.CausalContext(ctx)
		write(cctx, id)
		if after := insert(); after != at+size+short && after != at+size+long {
			id++
			continue // something else wrote too, or this write grew
		}
		var found bool
		err := m.Read(cctx, func(ctx context.Context, r data.ReadTx) error {
			var on bool
			if err := r.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+e.table+" WHERE id = $1), pg_is_in_recovery()", id).Scan(&found, &on); err != nil {
				return err
			}
			if !on {
				t.Error("the read ran on the primary")
			}
			return nil
		}, data.Consistency(data.ReadYourWrites.Wait(3*time.Second).OrFail()))
		if err != nil || !found {
			t.Fatalf("a write ending at a page, after %d tries: found %v, %v", try+1, found, err)
		}
		return
	}
	t.Fatal("no write could be made to end at a WAL page in 50 tries")
}
