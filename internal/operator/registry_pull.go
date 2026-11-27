package operator

import (
	"context"
	stderrors "errors"
	"fmt"

	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RegistryPullSecret is the tier copy of the organization's registry pull
// credential. Kyverno clones it into the organization's OpenChoreo
// environment namespaces, and the AEP component types reference it.
const RegistryPullSecret = "aep-registry-pull"

// registryPull delivers spec.policy.registryPullSecretRef to aep-system of the
// tier cluster, or removes the copy once the reference is gone.
func (s *session) registryPull(ctx context.Context, tier string) error {
	ref := s.o.Spec.Policy.RegistryPullSecretRef
	if ref == nil {
		// Nothing was delivered to a tier this session has not reached yet.
		c := s.tiers[tier]
		if c == nil {
			return nil
		}
		err := s.deleteNamed(ctx, c, "v1", "secrets", "aep-system", RegistryPullSecret)
		// Deletion was requested; the copy has no dependents to wait for.
		var p *pendingError
		if stderrors.As(err, &p) {
			return nil
		}
		return err
	}
	c, err := s.bootstrap(ctx, tier)
	if err != nil {
		return err
	}
	src, err := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return pending("registry pull Secret %s/%s not found", s.r.Options.Namespace, ref.Name)
	}
	if err != nil {
		return err
	}
	if src.Type != "kubernetes.io/dockerconfigjson" || len(src.Data[".dockerconfigjson"]) == 0 {
		return fmt.Errorf("registry pull Secret %s must be kubernetes.io/dockerconfigjson", ref.Name)
	}
	u := secretObject("aep-system", RegistryPullSecret, map[string][]byte{".dockerconfigjson": src.Data[".dockerconfigjson"]})
	u.Object["type"] = "kubernetes.io/dockerconfigjson"
	return c.apply(ctx, s.o, "secrets", u)
}
