package readapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/authn"
	"github.com/tym83/aep/internal/change"
	"github.com/tym83/aep/internal/compliance"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// WithComplianceNamespace selects the management namespace used by the operator collector.
func WithComplianceNamespace(namespace string) func(*Server) {
	return func(s *Server) { s.complianceNamespace = namespace }
}

type TierView struct {
	Tier     string               `json:"tier"`
	Stale    bool                 `json:"stale"`
	Snapshot *compliance.Snapshot `json:"snapshot"`
}

func complianceRoles(id *authn.Identity, o *api.Organization) []string {
	roles := []string{}
	canonical := memberships(id.Groups)[o.Name]
	for _, role := range []string{"developer", "platform", "admin", "auditor", "approver"} {
		// Keep canonical groups consistent with the main read API; additionally
		// honor explicit external-group grants for compliance.
		allowed := contains(canonical, role)
		for _, group := range o.Spec.Access[role] {
			allowed = allowed || change.HasGroup(id.Groups, group)
		}
		if allowed {
			roles = append(roles, role)
		}
	}
	return roles
}
func (s *Server) compliance(w http.ResponseWriter, r *http.Request, id *authn.Identity) {
	o := &api.Organization{}
	if err := s.client.Get(r.Context(), types.NamespacedName{Name: r.PathValue("org")}, o); err != nil {
		if apierrors.IsNotFound(err) {
			fail(w, 403, "Organization access denied")
		} else {
			fail(w, 503, "Organization lookup unavailable")
		}
		return
	}
	roles := complianceRoles(id, o)
	allowed := false
	for _, role := range []string{"developer", "platform", "admin", "auditor"} {
		allowed = allowed || slices.Contains(roles, role)
	}
	if !allowed {
		fail(w, 403, "Organization access denied")
		return
	}
	tiers := []string{"nonprod"}
	if o.Spec.Profile != "small" {
		tiers = append(tiers, "prod")
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	out := []TierView{}
	for _, tier := range tiers {
		cm := &corev1.ConfigMap{}
		err := s.client.Get(r.Context(), types.NamespacedName{Namespace: s.complianceNamespace, Name: "compliance-" + o.Name + "-" + tier}, cm)
		if apierrors.IsNotFound(err) {
			out = append(out, TierView{Tier: tier, Stale: true})
			continue
		}
		if err != nil {
			fail(w, 503, "Compliance snapshot unavailable")
			return
		}
		if cm.Labels["aep.aenix.io/organization"] != string(o.UID) {
			fail(w, 503, "Compliance snapshot ownership mismatch")
			return
		}
		var snapshot compliance.Snapshot
		if json.Unmarshal([]byte(cm.Data["snapshot.json"]), &snapshot) != nil || snapshot.Tier != tier || snapshot.CollectedAt.IsZero() {
			fail(w, 503, "Invalid compliance snapshot")
			return
		}
		out = append(out, TierView{Tier: tier, Stale: now.Sub(snapshot.CollectedAt) > 6*time.Minute, Snapshot: &snapshot})
	}
	write(w, 200, map[string]any{"organization": o.Name, "items": out})
}
