package datatest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
	"github.com/thec1oud/galleon/coord/local"
	"github.com/thec1oud/galleon/data"
	"github.com/thec1oud/galleon/errs"
	"github.com/thec1oud/galleon/pg"
	"github.com/thec1oud/galleon/testkit"
	"github.com/thec1oud/galleon/testkit/conformance/datatest"
)

// pgOptions installs package pg as the suite asks.
func pgOptions(t *testing.T, s datatest.Setup) []galleon.Option {
	leaks := "error"
	if s.LogLeaks {
		leaks = "log"
	}
	cfg, err := json.Marshal(map[string]any{"pg": map[string]any{
		"primary":       map[string]any{"dsn": s.DSN, "max_conns": 4},
		"named":         map[string]any{"other": map[string]any{"dsn": s.DSN, "max_conns": 2}},
		"leaks":         leaks,
		"retry_backoff": 1_000_000, // 1ms
	}})
	if err != nil {
		t.Fatal(err)
	}
	return []galleon.Option{galleon.Modules(pg.Module()), galleon.ConfigJSON(cfg)}
}

func TestConformance(t *testing.T) {
	datatest.Run(t, datatest.Factory{Options: pgOptions})
}

// A pool closing under requests refuses them as Unavailable, never with a
// panic or a race.
func TestStopUnderLoad(t *testing.T) {
	db := testkit.DB(t)
	m, err := pg.New(pg.Config{Primary: pg.Source{DSN: db.DSN(), MaxConns: 4}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
					var x int
					return r.QueryRow(ctx, "SELECT 1").Scan(&x)
				})
				if err != nil && errs.KindOf(err) != errs.Unavailable {
					t.Errorf("while stopping: %v", err)
				}
				_ = m.Ping(ctx, "")
				_ = m.Pool("")
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := m.Stop(sctx); err != nil {
		t.Error(err)
	}
	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()
	if _, err := m.BeginRead(ctx); errs.KindOf(err) != errs.Unavailable {
		t.Fatalf("after Stop: %v", err)
	}
	if err := m.Ping(ctx, ""); errs.KindOf(err) != errs.Unavailable {
		t.Fatalf("Ping after Stop: %v", err)
	}
}

// An app given a Manager with pg.Use neither starts nor stops it: the caller
// owns its pools before, during and after the app.
func TestUseLeavesTheManagerToItsOwner(t *testing.T) {
	db := testkit.DB(t)
	name := fmt.Sprintf("use_probe_%d", time.Now().UnixNano())
	dsn := withAppName(t, db.DSN(), name)
	m, err := pg.New(pg.Config{Primary: pg.Source{DSN: dsn, MaxConns: 4}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	if err := m.Start(ctx); err == nil {
		t.Fatal("a second Start worked")
	}
	conns := func() int {
		t.Helper()
		// Connections pgxpool opens in the background need a moment.
		time.Sleep(50 * time.Millisecond)
		n := 0
		if err := m.Read(ctx, func(ctx context.Context, r data.ReadTx) error {
			return r.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE application_name = $1", name).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := conns()

	app, err := galleon.New(galleon.Name("probe"), galleon.Coordination(local.New()), galleon.Deploy(galleon.SingleNode), galleon.DrainGrace(0),
		galleon.Modules(pg.Module(pg.Use(m))))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := galleon.MustResolve[*pg.Manager](app); got != m {
		t.Fatal("the app has another Manager")
	}
	if during := conns(); during > before {
		t.Errorf("the app opened pools of its own: %d connections, %d before", during, before)
	}
	if err := app.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Write(ctx, func(ctx context.Context, w data.WriteTx) error {
		_, err := w.Exec(ctx, "SELECT 1")
		return err
	}); err != nil {
		t.Fatalf("after the app stopped: %v", err)
	}
	if after := conns(); after == 0 {
		t.Error("the app closed the caller's pools")
	}
}

// withAppName sets application_name in dsn, in either DSN form.
func withAppName(t *testing.T, dsn, name string) string {
	t.Helper()
	if !strings.Contains(strings.SplitN(dsn, " ", 2)[0], "://") {
		return dsn + " application_name=" + name
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("application_name", name)
	u.RawQuery = q.Encode()
	return u.String()
}
