package datatest_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
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
