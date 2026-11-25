package operator

import (
	"context"
	api "github.com/tym83/aep/api/v1alpha1"
)

func (s *session) tenants(ctx context.Context) error {
	qs, total, e := QuotaAllocation(s.o.Spec.Quotas, len(Tiers(s.o)))
	if e != nil {
		return e
	}
	type item struct {
		ns, name string
		q        api.Quotas
	}
	items := []item{{"tenant-root", s.o.Name, total}}
	for i, t := range Tiers(s.o) {
		items = append(items, item{TenantNamespace(s.o.Name, ""), t, qs[i]})
	}
	for _, it := range items {
		q := TenantResourceQuotas(it.q)
		u := obj(appsAPI, "Tenant", it.ns, it.name, M{"gateway": false, "ingress": false, "etcd": false, "monitoring": false, "seaweedfs": false, "host": "", "resourceQuotas": q})
		if e := s.r.Management.apply(ctx, s.o, "tenants", u); e != nil {
			return e
		}
		got, e := s.r.Management.get(ctx, appsAPI, "tenants", it.ns, it.name)
		if e != nil {
			return e
		}
		if !ready(got) {
			if msg := failureMessage(got); msg != "" {
				return pending("Tenant %s/%s is not Ready: %s", it.ns, it.name, msg)
			}
			return pending("waiting for Tenant %s/%s Ready", it.ns, it.name)
		}
	}
	return nil
}

// TenantResourceQuotas converts organization quotas into Cozystack tenant
// resourceQuotas keys. Cozystack renders each key as requests/limits of a
// ResourceQuota, so GPUs must use the extended resource name, and a zero GPU
// quota is omitted rather than rendered as an invalid standard resource.
func TenantResourceQuotas(q api.Quotas) M {
	out := M{}
	for k, v := range q {
		if k == "gpu" {
			if v == "" || v == "0" {
				continue
			}
			k = "nvidia.com/gpu"
		}
		out[k] = v
	}
	return out
}

// failureMessage returns the message of a False Ready condition, so the
// organization status shows why a dependent object is stuck.
func failureMessage(u interface{ UnstructuredContent() map[string]any }) string {
	status, _ := u.UnstructuredContent()["status"].(map[string]any)
	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" && m["status"] == "False" {
			msg, _ := m["message"].(string)
			if len(msg) > 300 {
				msg = msg[:300]
			}
			return msg
		}
	}
	return ""
}
