package operator

import "testing"

func TestReadCredentialsRequired(t *testing.T) {
	for _, name := range []string{"", "git", "INVALID"} {
		o := argoOrg()
		o.Spec.VCS.ReadCredentialsSecretRef.Name = name
		if validateIntegrations(o) == nil {
			t.Fatalf("accepted unsafe read Secret reference %q", name)
		}
	}
	o := argoOrg()
	if e := validateIntegrations(o); e != nil {
		t.Fatal(e)
	}
}
