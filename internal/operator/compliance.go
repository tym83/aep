package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/compliance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
)

func parseExpiry(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

type policyMaintenance struct{ r *Reconciler }

func (m *policyMaintenance) NeedLeaderElection() bool { return true }
func (m *policyMaintenance) Start(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		organizations := &api.OrganizationList{}
		if err := m.r.List(ctx, organizations); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "list organizations for policy maintenance")
			continue
		}
		for i := range organizations.Items {
			o := &organizations.Items[i]
			if !o.DeletionTimestamp.IsZero() {
				continue
			}
			s := &session{r: m.r, o: o, tiers: map[string]*Cluster{}}
			for _, tier := range Tiers(o) {
				// Maintain tiers with a baseline release, independently of provisioning readiness.
				hr, err := m.r.Management.get(ctx, helmAPI, "helmreleases", TenantNamespace(o.Name, tier), "aep-app-cluster-baseline")
				if apierrors.IsNotFound(err) {
					continue
				}
				if err == nil && hr.GetLabels()[OwnerLabel] != string(o.UID) {
					err = fmt.Errorf("baseline release ownership mismatch")
				}
				if err == nil {
					c, e := s.bootstrap(ctx, tier)
					err = e
					if err == nil {
						if e := cleanExceptions(ctx, c, metav1.Now()); e != nil {
							ctrl.LoggerFrom(ctx).Error(e, "expire policy exceptions", "organization", o.Name, "tier", tier)
						}
						err = s.collectCompliance(ctx, c, tier, time.Now().UTC())
					}
				}
				if err != nil {
					ctrl.LoggerFrom(ctx).Error(err, "collect compliance", "organization", o.Name, "tier", tier)
				}
			}
		}
	}
}

func (s *session) collectCompliance(ctx context.Context, c *Cluster, tier string, now time.Time) error {
	name := "compliance-" + s.o.Name + "-" + tier
	old, err := s.r.Management.get(ctx, "v1", "configmaps", s.r.Options.Namespace, name)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		if old.GetLabels()[OwnerLabel] != string(s.o.UID) {
			return fmt.Errorf("compliance snapshot ownership mismatch")
		}
		data, _, _ := unstructured.NestedString(old.Object, "data", "snapshot.json")
		var previous compliance.Snapshot
		if json.Unmarshal([]byte(data), &previous) == nil && now.Sub(previous.CollectedAt) < 5*time.Minute {
			return nil
		}
	}
	reports := []unstructured.Unstructured{}
	for _, resource := range []string{"policyreports", "clusterpolicyreports"} {
		items, err := c.list(ctx, "wgpolicyk8s.io/v1alpha2", resource, "")
		if err != nil {
			return err
		} // Preserve the previous snapshot and its timestamp on partial failure.
		reports = append(reports, items...)
	}
	data, err := json.Marshal(compliance.Aggregate(tier, reports, now))
	if err != nil {
		return err
	}
	if len(data) > 900000 {
		return fmt.Errorf("compliance snapshot exceeds ConfigMap budget")
	}
	cm := obj("v1", "ConfigMap", s.r.Options.Namespace, name, nil)
	cm.Object["data"] = M{"snapshot.json": string(data)}
	return s.r.Management.apply(ctx, s.o, "configmaps", cm)
}
