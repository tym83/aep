package itsm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type deliveryFunc func(context.Context, Event, string) (string, error)

func (f deliveryFunc) Deliver(c context.Context, e Event, r string) (string, error) {
	return f(c, e, r)
}
func TestSignedWebhookReplayAndScope(t *testing.T) {
	secret := []byte("012345678901234567890123456789abcd")
	calls := 0
	engine := &Engine{Store: testStore(t), Adapter: deliveryFunc(func(context.Context, Event, string) (string, error) { calls++; return "", nil })}
	receiver := &Receiver{Engine: engine, Secret: secret, Organization: "acme"}
	event := NewEvent("change.note", "note", Note{Organization: "acme", Change: "change1", Text: "evidence"})
	for _, test := range []struct {
		name   string
		offset time.Duration
		tamper bool
		org    string
		want   int
	}{{"valid", 0, false, "acme", 204}, {"replay", 0, false, "acme", 204}, {"expired", -6 * time.Minute, false, "acme", 401}, {"future", 6 * time.Minute, false, "acme", 401}, {"tampered", 0, true, "acme", 401}, {"cross-org", 0, false, "other", 400}} {
		t.Run(test.name, func(t *testing.T) {
			e := event
			if test.org != "acme" {
				e = NewEvent("change.note", "note", Note{Organization: test.org, Change: "change1", Text: "evidence"})
			}
			raw, _ := json.Marshal(e)
			ts := strconv.FormatInt(time.Now().Add(test.offset).Unix(), 10)
			sig := Signature(secret, ts, raw)
			if test.tamper {
				raw = append(raw, ' ')
			}
			r := httptest.NewRequest("POST", "/v1/events", bytes.NewReader(raw))
			r.Header.Set("X-AEP-Timestamp", ts)
			r.Header.Set("X-AEP-Signature", sig)
			w := httptest.NewRecorder()
			receiver.ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatalf("got %d want %d", w.Code, test.want)
			}
		})
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
func TestGenericWebhookSigning(t *testing.T) {
	secret := []byte("012345678901234567890123456789abcd")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-AEP-Signature") != Signature(secret, r.Header.Get("X-AEP-Timestamp"), raw) {
			t.Error("bad signature")
		}
		var e Event
		if json.Unmarshal(raw, &e) != nil || e.Validate() != nil || r.Header.Get("Idempotency-Key") != e.ID {
			t.Error("invalid envelope")
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	vendor := &Webhook{URL: srv.URL, Secret: secret}
	if _, err := vendor.Deliver(context.Background(), changeEvent("Pending"), ""); err != nil {
		t.Fatal(err)
	}
}
