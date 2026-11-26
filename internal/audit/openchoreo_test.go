package audit

import (
	"context"
	"testing"
	"time"
)

const auditRecord = `{"time":"2026-11-26T21:24:00Z","level":"INFO","msg":"AUDIT-LOG","schema_version":"1","event_id":"e-1","event_time":"2026-11-26T21:24:00.1Z","actor":{"type":"user","id":"acme-developer"},"action":"project.update","category":"write","result":"success","request_id":"r-1","resource":{"type":"project","namespace":"acme","name":"logdemo"}}`

func TestAuditLineSelectsOnlyAuditRecords(t *testing.T) {
	if _, ok := auditLine([]byte(auditRecord)); !ok {
		t.Fatal("audit record not selected")
	}
	for _, line := range []string{`{"msg":"ACCESS-LOG","status":403}`, `not json`, `{"msg":"UpdateProject called"}`} {
		if _, ok := auditLine([]byte(line)); ok {
			t.Fatalf("selected %s", line)
		}
	}
}

func TestOpenChoreoRecordKeepsHumanIdentity(t *testing.T) {
	e, ok := OpenChoreoRecord("openchoreo", []byte(auditRecord))
	if !ok {
		t.Fatal("record rejected")
	}
	if e.Actor != "acme-developer" || e.Organization != "acme" || e.Resource != "project/logdemo" || e.Action != "project.update" || e.Details["result"] != "success" {
		t.Fatalf("%+v", e)
	}
	if again, _ := OpenChoreoRecord("openchoreo", []byte(auditRecord)); again.ID != e.ID {
		t.Fatal("event ID is not stable")
	}
	for _, bad := range []string{`{"event_id":"x","action":"a"}`, `{"event_id":"x","action":"a","result":"ok","event_time":"yesterday"}`} {
		if _, ok := OpenChoreoRecord("openchoreo", []byte(bad)); ok {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestRecentDropsOverlapDuplicates(t *testing.T) {
	r := &recent{seen: map[string]bool{}, limit: 2}
	if !r.add("a") || r.add("a") || !r.add("b") || !r.add("c") || !r.add("a") {
		t.Fatal("bounded duplicate tracking broken")
	}
}

func TestFollowStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		FollowOpenChoreoAudit(ctx, fakeClient(), "ns", "app=api", "api", "openchoreo", time.Minute, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector ignored cancellation")
	}
}
