package itsm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	api "github.com/tym83/aep/api/v1alpha1"
)

type ServiceNow struct{ HTTP HTTP }

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
	fields := map[string]any{"correlation_id": correlation, "type": string(cr.Spec.Model), "short_description": "AEP " + c.ID + " " + string(cr.Spec.Operation), "description": cr.Spec.Justification + "\nConfirm in AEP: " + c.ConfirmationURL}
	if cr.Spec.Window != nil {
		fields["start_date"] = cr.Spec.Window.Start.UTC().Format("2006-01-02 15:04:05")
		fields["end_date"] = cr.Spec.Window.End.UTC().Format("2006-01-02 15:04:05")
	}
	// Do not set approval=approved from AEP: CAB/ECAB owns the ITSM approval.
	switch cr.Status.Phase {
	case api.PhaseApplied:
		fields["state"] = "3"
		fields["close_code"] = "successful"
		fields["close_notes"] = "AEP applied the change"
	case api.PhaseFailed:
		fields["state"] = "3"
		fields["close_code"] = "unsuccessful"
		fields["close_notes"] = string(e.Data)
	case api.PhaseRejected, api.PhaseExpired, api.PhaseSuperseded:
		fields["state"] = "4"
		fields["close_notes"] = string(cr.Status.Phase)
	}
	if ref != "" {
		if !validID(ref) {
			return ref, errors.New("invalid ServiceNow sys_id")
		}
		return ref, s.HTTP.Do(ctx, "PATCH", "/api/now/table/change_request/"+ref, e.ID, fields, nil)
	}
	fields["state"] = "-5"
	// First create the record, then apply terminal state on the next delivery.
	var out struct {
		Result struct {
			ID string `json:"sys_id"`
		} `json:"result"`
	}
	if err := s.HTTP.Do(ctx, "POST", "/api/now/table/change_request", e.ID, fields, &out); err != nil {
		return "", err
	}
	if !validID(out.Result.ID) {
		return "", errors.New("ServiceNow omitted sys_id")
	}
	return s.change(ctx, e, c, out.Result.ID)
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
