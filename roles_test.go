package galleon_test

import (
	"strings"
	"testing"

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
