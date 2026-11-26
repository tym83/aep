package itsm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	api "github.com/tym83/aep/api/v1alpha1"
)

type ServiceNow struct {
	HTTP             HTTP
	StandardTemplate string
}

func (s *ServiceNow) Deliver(ctx context.Context, e Event, ref string) (string, error) {
	switch e.Type {
	case "change.created", "change.updated":
		var c Change
		if err := json.Unmarshal(e.Data, &c); err != nil {
			return ref, err
		}
		return s.change(ctx, e, c, ref)
	case "cmdb.sync", "ci.upserted", "ci.retired":
		var g Graph
		if err := json.Unmarshal(e.Data, &g); err != nil {
			return ref, err
		}
		if e.Type == "ci.retired" {
			for i := range g.Items {
				g.Items[i].Retired = true
			}
		}
		payload, err := IREPayload(g)
		if err != nil {
			return ref, err
		}
		var out struct {
			Result struct {
				HasError  bool `json:"hasError"`
				Relations []struct {
					Errors []json.RawMessage `json:"errors"`
				} `json:"relations"`
				Items []struct {
					Errors []json.RawMessage `json:"errors"`
				} `json:"items"`
			} `json:"result"`
		}
		err = s.HTTP.Do(ctx, "POST", "/api/now/identifyreconcile?sysparm_data_source=AEP", e.ID, payload, &out)
		if err == nil {
			if out.Result.HasError {
				return ref, errors.New("IRE rejected the graph")
			}
			for _, rel := range out.Result.Relations {
				if len(rel.Errors) > 0 {
					return ref, errors.New("IRE rejected a relationship")
				}
			}
			if len(out.Result.Items) != len(g.Items) {
				return ref, errors.New("IRE did not acknowledge every CI")
			}
			for _, item := range out.Result.Items {
				if len(item.Errors) > 0 {
					return ref, errors.New("IRE rejected a CI")
				}
			}
		}
		return ref, err
	case "alert.raised", "alert.resolved":
		var a Alert
		if err := json.Unmarshal(e.Data, &a); err != nil {
			return ref, err
		}
		if a.Key == "" {
			return ref, errors.New("alert key required")
		}
		severity := a.Severity
		if e.Type == "alert.resolved" {
			severity = "0"
		}
		if severity == "" {
			severity = "3"
		}
		payload := map[string]any{"records": []any{map[string]string{"source": "AEP", "event_class": "AEP", "message_key": a.Key, "node": a.Node, "description": a.Summary, "severity": severity, "type": "aep.alert"}}}
		return ref, s.HTTP.Do(ctx, "POST", "/api/global/em/jsonv2", e.ID, payload, nil)
	default:
		return ref, fmt.Errorf("ServiceNow does not support %s", e.Type)
	}
}
func (s *ServiceNow) change(ctx context.Context, e Event, c Change, ref string) (string, error) {
	if c.Request == nil || c.ID == "" {
		return ref, errors.New("change request and ID required")
	}
	cr := c.Request
	switch cr.Spec.Model {
	case api.ModelNormal, api.ModelStandard, api.ModelEmergency:
	default:
		return ref, errors.New("invalid change model")
	}
	// Correlation is a hash, never an encoded-query expression supplied by callers.
	correlation := "aep-" + Digest([]byte(c.ID))
	if ref == "" {
		var found struct {
			Result []struct {
				ID string `json:"sys_id"`
			} `json:"result"`
		}
		if err := s.HTTP.Do(ctx, "GET", "/api/now/table/change_request?sysparm_query="+url.QueryEscape("correlation_id="+correlation)+"&sysparm_fields=sys_id&sysparm_limit=2", "", nil, &found); err != nil {
			return ref, err
		}
		if len(found.Result) > 1 {
			return ref, errors.New("duplicate ServiceNow correlation ID")
		}
		if len(found.Result) == 1 {
			ref = found.Result[0].ID
		}
	}

	base := "/api/sn_chg_rest/change/" + string(cr.Spec.Model)
	fields := map[string]any{"correlation_id": correlation, "short_description": bounded("AEP "+c.ID+" "+string(cr.Spec.Operation), 160), "description": ShortText(c)}
	if cr.Spec.Window != nil {
		fields["start_date"] = cr.Spec.Window.Start.UTC().Format("2006-01-02 15:04:05")
		fields["end_date"] = cr.Spec.Window.End.UTC().Format("2006-01-02 15:04:05")
	}
	if ref == "" {
		path := base
		if cr.Spec.Model == api.ModelStandard {
			if !validID(s.StandardTemplate) {
				return ref, &HTTPError{400}
			}
			path += "/" + s.StandardTemplate
		}
		var out struct {
			Result snRecord `json:"result"`
		}
		if err := s.HTTP.Do(ctx, "POST", path, e.ID, fields, &out); err != nil {
			return ref, err
		}
		ref = string(out.Result.ID)
	}
	if !validID(ref) {
		return ref, errors.New("ServiceNow omitted sys_id")
	}
	path := base + "/" + ref
	if err := s.HTTP.Do(ctx, "PATCH", path, e.ID+"-text", fields, nil); err != nil {
		return ref, err
	}
	target := ""
	switch cr.Status.Phase {
	case api.PhaseApproved:
		target = "scheduled"
	case api.PhaseApplying:
		target = "implement"
	case api.PhaseApplied, api.PhaseFailed:
		target = "closed"
	case api.PhaseRejected, api.PhaseExpired, api.PhaseSuperseded:
		target = "canceled"
	}
	if target == "" {
		return ref, nil
	}
	for range 10 {
		var current struct {
			Result snRecord `json:"result"`
		}
		if err := s.HTTP.Do(ctx, "GET", path, "", nil, &current); err != nil {
			return ref, err
		}
		var next struct {
			Result struct {
				States      []string          `json:"available_states"`
				Labels      map[string]string `json:"state_label"`
				Transitions [][]struct {
					From      string `json:"from_state"`
					To        string `json:"to_state"`
					Available bool   `json:"transition_available"`
				} `json:"state_transitions"`
			} `json:"result"`
		}
		if err := s.HTTP.Do(ctx, "GET", "/api/sn_chg_rest/change/"+ref+"/nextstates", "", nil, &next); err != nil {
			return ref, err
		}
		state := string(current.Result.State)
		label := strings.ToLower(next.Result.Labels[state])
		if label == target || (target == "scheduled" && label == "implement") {
			return ref, nil
		}
		rank := map[string]int{"new": 0, "assess": 1, "authorize": 2, "scheduled": 3, "implement": 4, "review": 5, "closed": 6}
		selected := ""
		best := 100
		for _, value := range next.Result.States {
			name := strings.ToLower(next.Result.Labels[value])
			if value == state {
				continue
			}
			allowed := len(next.Result.Transitions) == 0
			for _, group := range next.Result.Transitions {
				for _, tr := range group {
					if tr.From == state && tr.To == value && tr.Available {
						allowed = true
					}
				}
			}
			if !allowed {
				continue
			}
			if target == "canceled" {
				if name == target {
					selected = value
					break
				}
				continue
			}
			r, ok := rank[name]
			cur, known := rank[label]
			if ok && known && r > cur && r <= rank[target] && r < best {
				selected = value
				best = r
			}
		}
		if selected == "" {
			return ref, errors.New("waiting for allowed ServiceNow transition")
		}
		// The Change Management API accepts state names, unlike the Table API.
		update := map[string]any{"state": strings.ToLower(next.Result.Labels[selected])}
		if update["state"] == "closed" {
			update["close_code"] = "successful"
			if cr.Status.Phase == api.PhaseFailed {
				update["close_code"] = "unsuccessful"
			}
			update["close_notes"] = ShortText(c)
		}
		if update["state"] == "canceled" {
			update["close_notes"] = ShortText(c)
		}
		if err := s.HTTP.Do(ctx, "PATCH", path, e.ID+"-"+selected, update, nil); err != nil {
			return ref, err
		}
	}
	return ref, errors.New("ServiceNow transition limit exceeded")
}

// Change Management fields are objects; Table API lookup fields are strings.
type snValue string

func (v *snValue) UnmarshalJSON(raw []byte) error {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		*v = snValue(value)
		return nil
	}
	var field struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &field); err != nil {
		return err
	}
	*v = snValue(field.Value)
	return nil
}

type snRecord struct {
	ID    snValue `json:"sys_id"`
	State snValue `json:"state"`
}

var ciClasses = map[string]string{"organization": "cmdb_ci_service_discovered", "application-service": "cmdb_ci_service_discovered", "cluster": "cmdb_ci_kubernetes_cluster", "database": "cmdb_ci_db_instance", "business-application": "cmdb_ci_business_app", "technical-service": "cmdb_ci_service_technical", "service-offering": "service_offering"}

func IREPayload(g Graph) (map[string]any, error) {
	if err := ValidateGraph(g); err != nil {
		return nil, err
	}
	indexes := map[string]int{}
	items := []any{}
	relations := []any{}
	for i, ci := range g.Items {
		class, ok := ciClasses[ci.Kind]
		if !ok || ci.UID == "" || ci.Name == "" {
			return nil, fmt.Errorf("invalid CI %q", ci.UID)
		}
		if _, exists := indexes[ci.UID]; exists {
			return nil, errors.New("duplicate CI UID")
		}
		indexes[ci.UID] = i
		values := map[string]any{"name": ci.Name, "discovery_source": "AEP"}
		if ci.Kind == "business-application" {
			if !validID(ci.ExternalID) {
				return nil, errors.New("Business Application must reference a customer-owned sys_id")
			}
			values = map[string]any{"sys_id": ci.ExternalID}
		}
		if ci.Retired {
			if ci.Kind == "business-application" {
				return nil, errors.New("cannot retire customer Business Application")
			}
			values["install_status"] = "7"
		}
		items = append(items, map[string]any{"className": class, "values": values, "sys_object_source_info": map[string]string{"source_name": "AEP", "source_native_key": ci.UID}})
	}
	for _, rel := range g.Relationships {
		p, pok := indexes[rel.Parent]
		c, cok := indexes[rel.Child]
		if !pok || !cok || p == c {
			return nil, errors.New("relationship endpoint missing or self-referential")
		}
		switch rel.Type {
		case "Depends on::Used by", "Runs on::Runs", "Contains::Contained by", "Consumes::Consumed by":
		default:
			return nil, errors.New("unsupported relationship type")
		}
		relations = append(relations, map[string]any{"parent": p, "child": c, "type": rel.Type})
	}
	if len(items) == 0 {
		return nil, errors.New("empty graph")
	}
	return map[string]any{"items": items, "relations": relations}, nil
}

// Approvals reads CAB/ECAB evidence without changing approval states.
func (s *ServiceNow) Approvals(ctx context.Context, changeID string) (json.RawMessage, error) {
	if !validID(changeID) {
		return nil, errors.New("invalid ServiceNow sys_id")
	}
	var out json.RawMessage
	err := s.HTTP.Do(ctx, "GET", "/api/now/table/sysapproval_approver?sysparm_query="+url.QueryEscape("sysapproval="+changeID)+"&sysparm_fields=sys_id,approver,state,sys_updated_on", "", nil, &out)
	return out, err
}
