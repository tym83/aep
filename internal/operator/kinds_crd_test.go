package operator

import (
	"os"
	"strings"
	"testing"

	"github.com/tym83/aep/internal/kinds"
)

// Every kind the bridge supports must be admissible in both tier service schemas.
func TestOrganizationCRDAllowsBridgeKinds(t *testing.T) {
	for _, path := range []string{"../../config/crd/aep.aenix.io_organizations.yaml", "../../charts/aep-operator/crds/aep.aenix.io_organizations.yaml"} {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		for kind := range kinds.Table {
			if n := strings.Count(string(b), `"`+kind+`"`); n < 2 {
				t.Errorf("%s: kind %s appears in %d allowedKinds enums, want 2", path, kind, n)
			}
		}
	}
}
