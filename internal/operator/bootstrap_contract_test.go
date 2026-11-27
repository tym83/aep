package operator

import "testing"

func TestContractRevision(t *testing.T) {
	a, e := contractRevision("acme")
	if e != nil {
		t.Fatal(e)
	}
	again, _ := contractRevision("acme")
	other, _ := contractRevision("globex")
	if a == "" || a != again {
		t.Fatalf("revision must be stable, got %q and %q", a, again)
	}
	// The aep-system Role names the org's build Secret, so the contract differs per org.
	if a == other {
		t.Fatalf("revision must depend on the org-scoped rules")
	}
}

func TestRoleScopesGrantTierBuildSecret(t *testing.T) {
	var named bool
	for _, r := range roleScopes("acme")["aep-system"] {
		m := r.(M)
		if names, ok := m["resourceNames"].([]any); ok && len(names) == 1 && names[0] == "vcs-acme-build" {
			named = true
		}
	}
	if !named {
		t.Fatal("aep-system scope must grant access to vcs-acme-build")
	}
}
