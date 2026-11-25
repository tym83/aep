package kinds

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMappings(t *testing.T) {
	for _, tt := range []struct {
		k, service, secret, release string
		port                        int32
	}{{"Postgres", "postgres-test-rw", "postgres-test-credentials", "postgres-test", 5432}, {"Redis", "rfrm-redis-test", "redis-test-auth", "redis-test", 6379}} {
		m := Table[tt.k]
		if m.Service("test") != tt.service || m.Secret("test") != tt.secret || m.Release("test") != tt.release || m.Port != tt.port {
			t.Fatal(tt)
		}
	}
}
func TestChartContracts(t *testing.T) {
	repo := os.Getenv("COZYSTACK_REPO")
	if repo == "" {
		t.Skip("set COZYSTACK_REPO to verify upstream chart contracts")
	}
	read := func(path string) string {
		t.Helper()
		if ref := os.Getenv("COZYSTACK_REF"); ref != "" {
			b, e := exec.Command("git", "-C", repo, "show", ref+":"+path).Output()
			if e != nil {
				t.Fatal(e)
			}
			return string(b)
		}
		b, e := os.ReadFile(filepath.Join(repo, path))
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	for _, tt := range []struct{ path, pattern string }{
		{"packages/apps/redis/templates/redisfailover.yaml", "{{ .Release.Name }}-auth"},
		{"packages/apps/redis/templates/dashboard-resourcemap.yaml", "rfrm-{{ .Release.Name }}"},
		{"packages/apps/postgres/templates/db.yaml", "-credentials"},
		{"packages/apps/postgres/templates/tenant-projection.yaml", "sourceKey: ca.crt"},
		{"packages/apps/postgres/templates/dashboard-resourcemap.yaml", "-rw"},
	} {
		if !strings.Contains(read(tt.path), tt.pattern) {
			t.Errorf("%s no longer contains %q", tt.path, tt.pattern)
		}
	}
}
