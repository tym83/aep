package operator

import (
	"context"
	"errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Visit every artifact even while another deletion is pending. Credentials are
// removed before uninstalling the chart that grants permission to delete them.
func (s *session) cleanupArgo(ctx context.Context) error {
	ns := TenantNamespace(s.o.Name, "nonprod")
	hr, err := s.r.Management.get(ctx, helmAPI, "helmreleases", ns, "aep-argocd")
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// Once the release is absent its credential Role may already be gone.
	if hr != nil {
		c, err := s.bootstrap(ctx, "nonprod")
		if err != nil {
			return err
		}
		var errs []error
		for _, name := range []string{"aep-oidc", "aep-corporate-vcs"} {
			if err := s.deleteNamed(ctx, c, "v1", "secrets", "argocd", name); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
	}
	return errors.Join(s.deleteNamed(ctx, s.r.Management, helmAPI, "helmreleases", ns, "aep-argocd"), s.deleteNamed(ctx, s.r.Management, "source.toolkit.fluxcd.io/v1", "helmrepositories", ns, "aep-argocd"))
}
func (s *session) cleanupBuildCredentials(ctx context.Context) error {
	return errors.Join(s.deleteNamed(ctx, s.r.Management, "v1", "secrets", s.r.Options.Namespace, "vcs-"+s.o.Name+"-build"), s.deleteNamed(ctx, s.cp, ocAPI, "secretreferences", s.o.Name, "aep-corporate-vcs"))
}
