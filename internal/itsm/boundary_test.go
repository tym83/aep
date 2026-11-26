package itsm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestInboundBoundaryAndRotation(t *testing.T) {
	secret := []byte(strings.Repeat("i", 32))
	old := []byte(strings.Repeat("p", 32))
	out := []byte(strings.Repeat("o", 32))
	calls := 0
	notes := &Engine{Store: testStore(t), Adapter: deliveryFunc(func(context.Context, Event, string) (string, error) { calls++; return "", &HTTPError{422} })}
	receiver := &Receiver{Keys: map[string][]byte{"new": secret, "old": old}, Organization: "acme", NoteEngine: notes}
	valid := Note{Organization: "acme", Change: "c", Text: "reference"}
	oversized := valid
	oversized.ApproverID = strings.Repeat("a", 256)
	escaped := valid
	escaped.ExternalID = strings.Repeat("\x01", 255)
	for _, tc := range []struct {
		kind, key string
		data      any
		secret    []byte
		want      int
	}{
		{"cmdb.sync", "new", sampleGraph(), secret, 400}, {"ci.upserted", "new", sampleGraph(), secret, 400}, {"ci.retired", "new", sampleGraph(), secret, 400},
		{"change.note", "new", oversized, secret, 400}, {"change.note", "new", escaped, secret, 422},
		{"change.note", "new", valid, out, 401}, {"change.note", "missing", valid, secret, 401},
		{"change.note", "new", valid, secret, 422}, {"change.note", "old", valid, old, 422},
	} {
		e := NewEvent(tc.kind, "ignored", tc.data)
		body, _ := json.Marshal(e)
		r := httptest.NewRequest("POST", "/v1/events", bytes.NewReader(body))
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		r.Header.Set("X-AEP-Timestamp", ts)
		r.Header.Set("X-AEP-Key-ID", tc.key)
		r.Header.Set("X-AEP-Signature", Signature(tc.secret, ts, body))
		w := httptest.NewRecorder()
		receiver.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s got %d want %d", tc.kind, w.Code, tc.want)
		}
	}
	if calls != 2 {
		t.Fatal("permanent note replay retried", calls)
	}
	w := httptest.NewRecorder()
	MetricsHandler(notes).ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `status="failed"} 2`) {
		t.Fatal(w.Body)
	}
}
func TestJiraBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "email@example.com" || p != "api-token" {
			t.Error("missing Basic credentials")
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	if err := (HTTP{BaseURL: srv.URL, BasicUser: "email@example.com", BasicToken: "api-token"}).Do(context.Background(), "GET", "/", "", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSeparateQueuesDoNotRetryEachOthersEvents(t *testing.T) {
	store := testStore(t)
	store.Scope = "vendor"
	vendorCalls, noteCalls := 0, 0
	vendor := &Engine{Store: store, Adapter: deliveryFunc(func(context.Context, Event, string) (string, error) { vendorCalls++; return "", &HTTPError{503} })}
	store.Scope = "aep"
	notes := &Engine{Store: store, Adapter: deliveryFunc(func(context.Context, Event, string) (string, error) { noteCalls++; return "", nil })}
	if err := vendor.Submit(context.Background(), changeEvent("Applied")); err == nil {
		t.Fatal("expected failure")
	}
	if err := notes.Retry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := notes.Submit(context.Background(), NewEvent("change.note", "note/acme/1", Note{})); err != nil {
		t.Fatal(err)
	}
	if noteCalls != 1 || vendorCalls != 1 {
		t.Fatal("queue isolation failed")
	}
}
func TestEscapedEvidenceLengthRejectedBeforeStorage(t *testing.T) {
	n := Note{Text: strings.Repeat("x", 3000), ExternalID: strings.Repeat("\x01", 255)}
	if validateNote(n) == nil {
		t.Fatal("escaped JSON exceeded AEP budget")
	}
	for _, n := range []Note{{Text: strings.Repeat("x", 3001)}, {Text: "ok", ExternalID: strings.Repeat("x", 256)}, {Text: "ok", ApproverID: strings.Repeat("x", 256)}} {
		if validateNote(n) == nil {
			t.Fatal("oversize field accepted")
		}
	}
}
func TestOAuthPermanentFailureIsTerminalAndRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"SECRET"}`))
	}))
	defer srv.Close()
	h := HTTP{BaseURL: srv.URL, Client: OAuthClient(context.Background(), srv.URL+"/token", "id", "secret")}
	err := h.Do(context.Background(), "GET", "/api", "", nil, nil)
	if permanent(err) != 401 || strings.Contains(err.Error(), "SECRET") {
		t.Fatal(err)
	}
}
