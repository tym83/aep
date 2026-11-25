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
		q := M{}
		for k, v := range it.q {
			q[k] = v
		}
		u := obj(appsAPI, "Tenant", it.ns, it.name, M{"gateway": false, "ingress": false, "etcd": false, "monitoring": false, "seaweedfs": false, "host": "", "resourceQuotas": q})
		if e := s.r.Management.apply(ctx, s.o, "tenants", u); e != nil {
			return e
		}
		got, e := s.r.Management.get(ctx, appsAPI, "tenants", it.ns, it.name)
		if e != nil {
			return e
		}
		if !ready(got) {
			return pending("waiting for Tenant %s/%s Ready", it.ns, it.name)
		}
	}
	return nil
}
