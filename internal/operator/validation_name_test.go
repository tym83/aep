package operator

import "testing"

func TestValidateName(t *testing.T) {
	for _, name := range []string{"platform", "security", "aep", "root", "cozy", "system", "team-one", "UPPER", ""} {
		if ValidateName(name) == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if err := ValidateName("acme"); err != nil {
		t.Fatal(err)
	}
}
