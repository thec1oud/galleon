package galleon_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The op plane and the transports know nothing of Postgres: none of them
// links a Postgres driver, even through the data package's seam.
func TestTheOpPlaneAndTransportsDontLinkPostgres(t *testing.T) {
	for _, pkg := range []string{"./op", "./server", "./client"} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Skipf("go list: %v", err)
		}
		for _, dep := range strings.Fields(string(out)) {
			if strings.HasPrefix(dep, "github.com/jackc/") {
				t.Errorf("%s depends on %s", pkg, dep)
			}
		}
	}
}
