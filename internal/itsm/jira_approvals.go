package itsm

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// jiraChangelogPages bounds how much issue history one poll reads.
const jiraChangelogPages = 10

// ApprovalDecisions reads two kinds of Jira evidence: decided JSM approval
// steps, and transitions into the statuses configured in DecisionStatuses
// (for projects whose CAB decides by moving the request), matched by status
// ID or, failing that, by the possibly localized status name. The transition
// author is the approver. Like ServiceNow, these are notes, never decisions.
func (j *Jira) ApprovalDecisions(ctx context.Context, issue string) ([]Approval, error) {
	if !validID(issue) {
		return nil, errors.New("invalid issue key")
	}
	var out []Approval
	var steps struct {
		Values []struct {
			ID            string `json:"id"`
			FinalDecision string `json:"finalDecision"`
			Approvers     []struct {
				Approver struct {
					AccountID string `json:"accountId"`
				} `json:"approver"`
				Decision string `json:"approverDecision"`
			} `json:"approvers"`
		} `json:"values"`
	}
	if err := j.HTTP.Do(ctx, "GET", "/rest/servicedeskapi/request/"+issue+"/approval", "", nil, &steps); err != nil {
		return nil, err
	}
	for _, s := range steps.Values {
		decision := jiraDecision(s.FinalDecision)
		if decision == "" || !validID(s.ID) {
			continue
		}
		for _, a := range s.Approvers {
			if jiraDecision(a.Decision) == decision && a.Approver.AccountID != "" {
				out = append(out, Approval{ID: issue + "-approval-" + s.ID + "-" + idPart(a.Approver.AccountID), Approver: a.Approver.AccountID, Decision: decision})
			}
		}
	}
	if len(j.DecisionStatuses) == 0 {
		return out, nil
	}
	for page, start := 0, 0; page < jiraChangelogPages; page++ {
		var log struct {
			Values []struct {
				ID     string `json:"id"`
				Author struct {
					AccountID string `json:"accountId"`
				} `json:"author"`
				Items []struct {
					Field    string `json:"field"`
					To       string `json:"to"`
					ToString string `json:"toString"`
				} `json:"items"`
			} `json:"values"`
			IsLast bool `json:"isLast"`
		}
		if err := j.HTTP.Do(ctx, "GET", "/rest/api/3/issue/"+issue+"/changelog?maxResults=100&startAt="+strconv.Itoa(start), "", nil, &log); err != nil {
			return nil, err
		}
		for _, h := range log.Values {
			for _, it := range h.Items {
				// Status names follow the reader's language; IDs do not.
				decision := j.DecisionStatuses[it.To]
				if decision == "" {
					decision = j.DecisionStatuses[it.ToString]
				}
				if it.Field != "status" || decision == "" || !validID(h.ID) || h.Author.AccountID == "" {
					continue
				}
				out = append(out, Approval{ID: issue + "-status-" + h.ID, Approver: h.Author.AccountID, Decision: decision})
			}
		}
		if log.IsLast || len(log.Values) == 0 {
			break
		}
		start += len(log.Values)
	}
	return out, nil
}

func jiraDecision(s string) string {
	return map[string]string{"approved": "approve", "declined": "reject"}[strings.ToLower(s)]
}

// idPart keeps an account ID usable inside a note subject.
func idPart(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, s)
}
