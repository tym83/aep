package itsm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJiraApprovalDecisions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/servicedeskapi/request/AEP-7/approval":
			fmt.Fprint(w, `{"values":[
				{"id":"11","finalDecision":"approved","approvers":[{"approver":{"accountId":"712020:abc"},"approverDecision":"approved"},{"approver":{"accountId":"x2"},"approverDecision":"pending"}]},
				{"id":"12","finalDecision":"pending","approvers":[{"approver":{"accountId":"x3"},"approverDecision":"approved"}]}]}`)
		case "/rest/api/3/issue/AEP-7/changelog":
			if r.URL.Query().Get("startAt") == "0" {
				fmt.Fprint(w, `{"isLast":false,"values":[{"id":"100","author":{"accountId":"cab1"},"items":[{"field":"status","toString":"Work in progress"}]},
					{"id":"101","author":{"accountId":"cab1"},"items":[{"field":"assignee","toString":"Approved"}]}]}`)
			} else {
				fmt.Fprint(w, `{"isLast":true,"values":[{"id":"102","author":{"accountId":"cab2"},"items":[{"field":"status","toString":"Declined"}]},
					{"id":"103","author":{"accountId":"cab3"},"items":[{"field":"status","to":"10012","toString":"Подтверждено"}]}]}`)
			}
		default:
			t.Error(r.URL)
		}
	}))
	defer srv.Close()
	j := &Jira{HTTP: HTTP{BaseURL: srv.URL}, Project: "AEP", DecisionStatuses: map[string]string{"10012": "approve", "Declined": "reject"}}
	got, err := j.ApprovalDecisions(context.Background(), "AEP-7")
	if err != nil {
		t.Fatal(err)
	}
	want := []Approval{
		{ID: "AEP-7-approval-11-712020_abc", Approver: "712020:abc", Decision: "approve"},
		{ID: "AEP-7-status-102", Approver: "cab2", Decision: "reject"},
		{ID: "AEP-7-status-103", Approver: "cab3", Decision: "approve"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for _, a := range got {
		if !validID(a.ID) {
			t.Fatal("note subject unsafe:", a.ID)
		}
	}
	if _, err = j.ApprovalDecisions(context.Background(), "AEP 7"); err == nil {
		t.Fatal("invalid key accepted")
	}
	var _ ApprovalReader = j
}
