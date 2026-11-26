package itsm

import (
	"context"
	"encoding/json"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSN rejects Table API writes, type changes and skipped state transitions.
// Normal includes Assess/Authorize; emergency skips Assess; standard starts Scheduled.
type fakeSN struct {
	records map[string]map[string]any
	lost    bool
	blocked bool
	closed  int
	patches int
}

var stateLabels = map[string]string{"-5": "New", "-4": "Assess", "-3": "Authorize", "-2": "Scheduled", "-1": "Implement", "0": "Review", "3": "Closed", "4": "Canceled"}

func (f *fakeSN) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if f.records == nil {
		f.records = map[string]map[string]any{}
	}
	if r.URL.Path == "/api/now/table/change_request" && r.Method == "GET" {
		result := []any{}
		query := strings.TrimPrefix(r.URL.Query().Get("sysparm_query"), "correlation_id=")
		for id, rec := range f.records {
			if rec["correlation_id"] == query {
				result = append(result, map[string]string{"sys_id": id})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/sn_chg_rest/change/") {
		http.Error(w, "wrong endpoint", 400)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sn_chg_rest/change/"), "/")
	if r.Method == "POST" {
		model := parts[0]
		if model != "normal" && model != "standard" && model != "emergency" {
			http.Error(w, "model", 400)
			return
		}
		if model == "standard" && (len(parts) != 2 || parts[1] != "template123") {
			http.Error(w, "template", 400)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["type"] != nil || body["state"] != nil {
			http.Error(w, "create state/type", 400)
			return
		}
		id := fmt.Sprintf("sn%d", len(f.records)+1)
		body["state"] = "-5"
		body["model"] = model
		if model == "standard" {
			body["state"] = "-2"
		}
		f.records[id] = body
		if f.lost {
			f.lost = false
			http.Error(w, "lost after commit", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"sys_id": map[string]string{"value": id}}})
		return
	}
	id := parts[len(parts)-1]
	if id == "nextstates" {
		id = parts[0]
	}
	rec := f.records[id]
	if rec == nil {
		http.NotFound(w, r)
		return
	}
	state := rec["state"].(string)
	next := map[string]string{"-5": "-4", "-4": "-3", "-3": "-2", "-2": "-1", "-1": "0", "0": "3"}[state]
	if state == "-5" && rec["model"] == "emergency" {
		next = "-3"
	}
	if parts[len(parts)-1] == "nextstates" {
		states := []string{state}
		transitions := [][]map[string]any{}
		for _, to := range []string{next, "4"} {
			if to == "" || state == "3" || state == "4" {
				continue
			}
			states = append(states, to)
			transitions = append(transitions, []map[string]any{{"from_state": state, "to_state": to, "transition_available": !f.blocked}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"available_states": states, "state_label": stateLabels, "state_transitions": transitions}})
		return
	}
	if r.Method == "GET" {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"sys_id": map[string]string{"value": id}, "state": map[string]string{"value": state}}})
		return
	}
	if r.Method != "PATCH" {
		http.Error(w, "method", 400)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body["type"] != nil || body["approval"] != nil {
		http.Error(w, "immutable type/approval", 400)
		return
	}
	if value, ok := body["state"].(string); ok {
		to := ""
		for k, v := range stateLabels {
			if strings.ToLower(v) == value {
				to = k
			}
		}
		if f.blocked || to == "" || (to != next && to != "4") || state == "3" || state == "4" {
			http.Error(w, "illegal transition", 409)
			return
		}
		if to == "3" {
			if state != "0" || body["close_notes"] == nil || (body["close_code"] != "successful" && body["close_code"] != "unsuccessful") {
				http.Error(w, "closure", 400)
				return
			}
			f.closed++
		}
		body["state"] = to
	}
	for k, v := range body {
		rec[k] = v
	}
	f.patches++
	fmt.Fprint(w, `{"result":{}}`)
}
func TestServiceNowModelsAndPhases(t *testing.T) {
	for _, model := range []api.ChangeModel{api.ModelNormal, api.ModelStandard, api.ModelEmergency} {
		for _, phase := range []api.ChangePhase{api.PhaseApproved, api.PhaseApplying, api.PhaseApplied, api.PhaseFailed, api.PhaseRejected, api.PhaseExpired, api.PhaseSuperseded} {
			t.Run(string(model)+"/"+string(phase), func(t *testing.T) {
				f := &fakeSN{}
				srv := newSNServer(t, f)
				s := &ServiceNow{HTTP: HTTP{BaseURL: srv}, StandardTemplate: "template123"}
				e := changeEvent(phase)
				var c Change
				_ = json.Unmarshal(e.Data, &c)
				c.Request.Spec.Model = model
				e.Data, _ = json.Marshal(c)
				ref, err := s.Deliver(context.Background(), e, "")
				if err != nil {
					t.Fatal(err)
				}
				want := map[api.ChangePhase]string{api.PhaseApproved: "-2", api.PhaseApplying: "-1", api.PhaseApplied: "3", api.PhaseFailed: "3", api.PhaseRejected: "4", api.PhaseExpired: "4", api.PhaseSuperseded: "4"}[phase]
				if f.records[ref]["state"] != want {
					t.Fatal(f.records)
				}
				if phase == api.PhaseFailed && f.records[ref]["close_code"] != "unsuccessful" {
					t.Fatal("wrong result")
				}
			})
		}
	}
}

func newSNServer(t *testing.T, f *fakeSN) string {
	t.Helper()
	s := httptest.NewServer(f)
	t.Cleanup(s.Close)
	return s.URL
}
func TestServiceNowWaitsForApprovalAndRequiresTemplate(t *testing.T) {
	f := &fakeSN{blocked: true}
	s := &ServiceNow{HTTP: HTTP{BaseURL: newSNServer(t, f)}}
	e := changeEvent(api.PhaseApplied)
	ref, err := s.Deliver(context.Background(), e, "")
	if err == nil || permanent(err) != 0 || f.records[ref]["state"] != "-5" {
		t.Fatal("approval bypass", err)
	}
	f.blocked = false
	if _, err = s.Deliver(context.Background(), e, ref); err != nil {
		t.Fatal(err)
	}
	var c Change
	_ = json.Unmarshal(e.Data, &c)
	c.ID = "standard"
	c.Request.Spec.Model = api.ModelStandard
	e.Data, _ = json.Marshal(c)
	if _, err = s.Deliver(context.Background(), e, ""); permanent(err) != 400 {
		t.Fatal("missing template accepted", err)
	}
}
func TestChangeTextDoesNotDiscloseRequest(t *testing.T) {
	e := changeEvent(api.PhaseFailed)
	var c Change
	_ = json.Unmarshal(e.Data, &c)
	c.Request.Spec.Justification = "SECRET-CONTENT"
	c.ConfirmationURL = strings.Repeat("x", 5000)
	text := ShortText(c)
	if strings.Contains(text, "SECRET") || len(text) > 1200 || !strings.Contains(text, "Revision:") {
		t.Fatal("unsafe summary")
	}
}
