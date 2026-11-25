package operator

import (
	"context"
	"fmt"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"time"
)

// Renew credentials independently of provisioning readiness so an unrelated outage cannot expire them.
func (s *session) renewCredentials(ctx context.Context) error {
	for _, tier := range Tiers(s.o) {
		name := "operator-access-" + s.o.Name + "-" + tier
		sec, e := s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(e) {
			continue
		}
		if e != nil {
			return e
		}
		if sec.Labels[OwnerLabel] != string(s.o.UID) {
			return fmt.Errorf("credential ownership mismatch")
		}
		cfg, e := clientcmd.RESTConfigFromKubeConfig(sec.Data["kubeconfig"])
		if e != nil {
			return e
		}
		c, e := NewCluster(cfg)
		if e != nil {
			return e
		}
		expiry, e := time.Parse(time.RFC3339, string(sec.Data["expires"]))
		if e != nil {
			return e
		}
		if time.Until(expiry) < 12*time.Hour {
			token, expires, e := requestToken(ctx, c, "aep-operator")
			if e != nil {
				return e
			}
			cfg.BearerToken = token
			if e = s.saveOperatorCredential(ctx, name, cfg, expires); e != nil {
				return e
			}
			c, e = NewCluster(cfg)
			if e != nil {
				return e
			}
		}
		s.tiers[tier] = c
		name = "aep-bridge-" + s.o.Name + "-" + tier
		sec, e = s.r.Management.Kube.CoreV1().Secrets(s.r.Options.Namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(e) {
			continue
		}
		if e != nil {
			return e
		}
		expiry, e = time.Parse(time.RFC3339, string(sec.Data["expires"]))
		if e != nil {
			return e
		}
		if time.Until(expiry) < 12*time.Hour {
			token, expires, e := requestToken(ctx, c, "aep-bridge")
			if e != nil {
				return e
			}
			sec.Data["token"] = []byte(token)
			sec.Data["expires"] = []byte(expires.Format(time.RFC3339))
			if e = s.r.Management.apply(ctx, s.o, "secrets", secretObject(sec.Namespace, sec.Name, sec.Data)); e != nil {
				return e
			}
		}
	}
	return nil
}
