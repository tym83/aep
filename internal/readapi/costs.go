package readapi

import (
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/authn"
	"net/http"
	"time"
)

func (s *Server) costAccess(w http.ResponseWriter, r *http.Request, id *authn.Identity) {
	rs := memberships(id.Groups)[r.PathValue("org")]
	allowed := false
	for _, role := range []string{"developer", "platform", "admin", "auditor", "finance"} {
		allowed = allowed || contains(rs, role)
	}
	if !allowed {
		fail(w, 403, "organization cost access denied")
		return
	}
	s.organization(s.costs, nil)(w, r, id)
}
func (s *Server) costs(w http.ResponseWriter, r *http.Request, org *api.Organization) {
	from, e := time.Parse(time.DateOnly, r.URL.Query().Get("from"))
	to, ee := time.Parse(time.DateOnly, r.URL.Query().Get("to"))
	group := r.URL.Query().Get("groupBy")
	if group == "" {
		group = "project"
	}
	if e != nil || ee != nil || !to.After(from) || to.Sub(from) > 366*24*time.Hour || (group != "project" && group != "service" && group != "tier") {
		fail(w, 400, "from/to must be YYYY-MM-DD, from < to (exclusive), at most 366 days; groupBy must be project, service or tier")
		return
	}
	if s.Costs == nil {
		fail(w, 503, "cost ledger is not configured")
		return
	}
	items, e := s.Costs.Query(r.Context(), org.Name, from, to, group)
	if e != nil {
		fail(w, 503, "cost ledger unavailable")
		return
	}
	write(w, 200, map[string]any{"items": items, "from": from.Format(time.DateOnly), "to": to.Format(time.DateOnly), "groupBy": group})
}
