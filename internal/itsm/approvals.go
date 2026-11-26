package itsm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	api "github.com/tym83/aep/api/v1alpha1"
)

// Approval is one CAB/ECAB decision recorded in the ITSM.
type Approval struct {
	ID, Approver, Decision string
}

// ApprovalReader reads the decisions recorded against a mirrored change.
type ApprovalReader interface {
	ApprovalDecisions(ctx context.Context, ref string) ([]Approval, error)
}

// ApprovalDecisions maps ServiceNow approval records to decisions; requested
// or not-yet-decided records are ignored.
func (s *ServiceNow) ApprovalDecisions(ctx context.Context, ref string) ([]Approval, error) {
	raw, err := s.Approvals(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result []struct {
			ID       snValue `json:"sys_id"`
			Approver snValue `json:"approver"`
			State    snValue `json:"state"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	var decisions []Approval
	for _, a := range out.Result {
		decision := map[string]string{"approved": "approve", "rejected": "reject"}[strings.ToLower(string(a.State))]
		if decision == "" || !validID(string(a.ID)) {
			continue
		}
		decisions = append(decisions, Approval{ID: string(a.ID), Approver: string(a.Approver), Decision: decision})
	}
	return decisions, nil
}

// pollApprovals forwards ITSM decisions on pending changes to AEP as evidence
// notes. They never decide: an eligible person still confirms in AEP. Note
// subjects and payloads are stable, so a decision is noted once.
func (p *Poller) pollApprovals(ctx context.Context, cr *api.ChangeRequest, id string) error {
	if p.Approvals == nil || p.Notes == nil || cr.Status.Phase != api.PhasePending {
		return nil
	}
	_, record, err := p.Engine.Store.get(ctx, "change/"+id)
	if err != nil || !validID(record.Ref) {
		return err
	}
	decisions, err := p.Approvals.ApprovalDecisions(ctx, record.Ref)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range decisions {
		note := Note{Organization: p.Organization, Change: cr.Name, ExternalID: d.ID, ApproverID: d.Approver, Decision: d.Decision,
			Text: "ITSM approval record " + d.Decision + "d for " + record.Ref + "; confirm the decision in AEP."}
		event := NewEvent("change.note", "note/"+p.Organization+"/itsm-"+d.ID+"-"+d.Decision, note)
		errs = append(errs, p.Notes.Submit(ctx, event))
	}
	return errors.Join(errs...)
}
