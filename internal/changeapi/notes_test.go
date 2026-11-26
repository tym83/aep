package changeapi

import (
	"encoding/json"
	"testing"
)

func TestITSMNotesIdempotency(t *testing.T) {
	s, _ := testServer(t)
	res := createChange(t, s, "alice", "note-test")
	var out changeView
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	path := "/v1/changes/acme/" + out.Request.Name + "/notes"
	for i, want := range []int{201, 200} {
		r := call(t, s, "POST", path, "sa", `{"text":"CAB approved; confirmation required"}`, map[string]string{"Idempotency-Key": "approval-1"})
		if r.Code != want {
			t.Fatalf("attempt %d: %d %s", i, r.Code, r.Body)
		}
	}
	r := call(t, s, "POST", path, "sa", `{"text":"changed"}`, map[string]string{"Idempotency-Key": "approval-1"})
	if r.Code != 409 {
		t.Fatal(r.Code)
	}
	r = call(t, s, "GET", "/v1/changes/acme/"+out.Request.Name, "sa", "", nil)
	_ = json.Unmarshal(r.Body.Bytes(), &out)
	if len(out.Request.Status.Notes) != 1 || out.Request.Status.Notes[0].Source != "itsm" {
		t.Fatalf("unexpected notes: %+v", out.Request.Status.Notes)
	}
}
