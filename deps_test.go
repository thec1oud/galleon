package galleon_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The kernel, the op plane and the transports know nothing of Postgres:
// none of them links a Postgres driver, even through the data package's
// seam.
func TestTheKernelTheOpPlaneAndTheTransportsDontLinkPostgres(t *testing.T) {
	for _, pkg := range []string{".", "./op", "./server", "./client"} {
		for _, dep := range deps(t, pkg) {
			if strings.HasPrefix(dep, "github.com/jackc/") {
				t.Errorf("%s depends on %s", pkg, dep)
			}
		}
	}
}

// The data seam links pgx's types, and of Galleon only the error model: an
// ORM adapter that imports it gets neither the kernel, nor package pg, nor
// a pool.
func TestTheDataSeamLinksOnlyPgxTypesAndErrors(t *testing.T) {
	for _, dep := range deps(t, "./data") {
		switch {
		case strings.HasPrefix(dep, "github.com/thec1oud/galleon") && dep != "github.com/thec1oud/galleon/data" && dep != "github.com/thec1oud/galleon/errs",
			strings.HasPrefix(dep, "github.com/jackc/pgx/v5/pgxpool"), strings.HasPrefix(dep, "github.com/jackc/puddle"):
			t.Errorf("data depends on %s", dep)
		}
	}
}

func deps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	return strings.Fields(string(out))
}
