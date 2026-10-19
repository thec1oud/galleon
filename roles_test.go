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
