package galleon_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thec1oud/galleon"
)

func TestRolesSelectPlanes(t *testing.T) {
	var web, worker bool
	mod := galleon.ModuleFunc("planes", func(r *galleon.Registry) error {
		web, worker = r.Active("web"), r.Active("worker")
		return nil
	})
	if _, err := galleon.New(base(mod)...); err != nil {
		t.Fatal(err)
	}
	if !web || !worker {
		t.Fatal("the default role runs everything")
	}
	app, err := galleon.New(append(base(mod), galleon.Roles("worker"))...)
	if err != nil {
		t.Fatal(err)
	}
	if web || !worker {
		t.Fatalf("web=%v worker=%v, want only worker", web, worker)
	}
	if got := app.Roles(); len(got) != 1 || got[0] != "worker" {
		t.Fatalf("Roles() = %v", got)
	}
}

func TestUnknownRoleFailsBoot(t *testing.T) {
	mod := galleon.ModuleFunc("planes", func(r *galleon.Registry) error {
		r.Active("worker")
		return nil
	})
	_, err := galleon.New(append(base(mod), galleon.Roles("wroker"))...)
	if err == nil || !strings.Contains(err.Error(), `role "wroker" is not used by any module`) {
		t.Fatalf("expected unknown role error, got %v", err)
	}
}

func TestActiveDeclaresEveryRolePassed(t *testing.T) {
	mod := galleon.ModuleFunc("api", func(r *galleon.Registry) error {
		r.Active("web", "rpc") // web matches first; rpc must still be declared
		return nil
	})
	if _, err := galleon.New(append(base(mod), galleon.Roles("web", "rpc"))...); err != nil {
		t.Fatal(err)
	}
}

func TestOnReadyRunsOnceAfterGatesPass(t *testing.T) {
	var open atomic.Bool
	var runs atomic.Int32
	mod := galleon.ModuleFunc("web", func(r *galleon.Registry) error {
		r.Gate("deps", func(context.Context) error {
			if !open.Load() {
				return errors.New("not yet")
			}
			return nil
		})
		r.OnReady(func(context.Context) error { runs.Add(1); return nil })
		return nil
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	time.Sleep(200 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatal("OnReady ran before the gate passed")
	}
	open.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("OnReady ran %d times, want 1", n)
	}
}

func TestFailingReadyHookIsFatal(t *testing.T) {
	mod := galleon.ModuleFunc("web", func(r *galleon.Registry) error {
		r.OnReady(func(context.Context) error { return errors.New("address already in use") })
		return nil
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = app.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "address already in use") || ctx.Err() != nil {
		t.Fatalf("Run should stop on its own and return the hook's error, got %v (ctx err %v)", err, ctx.Err())
	}
}

func TestReadyHookNeverRunsOnceShutdownBegins(t *testing.T) {
	var ran atomic.Bool
	mod := galleon.ModuleFunc("web", func(r *galleon.Registry) error {
		r.Gate("never", func(context.Context) error { return errors.New("not yet") })
		r.OnReady(func(context.Context) error { ran.Store(true); return nil })
		return nil
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Start(context.Background())
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if ran.Load() {
		t.Fatal("OnReady ran after shutdown")
	}
}

func TestShutdownDuringAReadyHookIsClean(t *testing.T) {
	entered := make(chan struct{})
	mod := galleon.ModuleFunc("web", func(r *galleon.Registry) error {
		r.OnReady(func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		})
		return nil
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	<-entered
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a hook cancelled by shutdown is not a failure, got %v", err)
	}
}

func TestShutdownIsNotHeldByABlockedReadyHook(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	mod := galleon.ModuleFunc("web", func(r *galleon.Registry) error {
		r.OnReady(func(context.Context) error { close(entered); <-release; return nil })
		return nil
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Start(context.Background())
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = app.Shutdown(ctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown took %v; it must not wait past its deadline for a hook", d)
	}
}

func TestReadyHookContextLastsUntilShutdown(t *testing.T) {
	got := make(chan context.Context, 1)
	mod := galleon.ModuleFunc("web", func(r *galleon.Registry) error {
		r.OnReady(func(ctx context.Context) error { got <- ctx; return nil })
		return nil
	})
	app, err := galleon.New(base(mod)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := <-got
	time.Sleep(100 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("the ready hook's context ended before Shutdown; servers using it as their base context would stop")
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("Shutdown must cancel the ready hook's context")
	}
}
