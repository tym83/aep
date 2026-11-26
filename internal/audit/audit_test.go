package audit

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func openTest(t *testing.T) *Spool {
	t.Helper()
	s, e := Open(t.TempDir(), "test", 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func event() Event {
	return Event{Source: "test", Action: "write", Actor: "alice", Resource: "Organization/acme"}
}
func TestChainAndRecovery(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir, "test", 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Append([]Event{event(), event()}); e != nil {
		t.Fatal(e)
	}
	p := s.Pending()
	b := JSONL(p)
	if _, e = Verify(bytes.NewReader(b), "", p[1].Hash); e != nil {
		t.Fatal(e)
	}
	bad := bytes.Replace(b, []byte("alice"), []byte("mallory"), 1)
	if _, e = Verify(bytes.NewReader(bad), "", ""); e == nil {
		t.Fatal("tampering accepted")
	}
	if _, e = Verify(bytes.NewReader(JSONL(p[1:])), "", ""); e == nil {
		t.Fatal("missing prefix accepted")
	}
	if _, e = Verify(bytes.NewReader(JSONL(p[:1])), "", p[1].Hash); e == nil {
		t.Fatal("missing suffix accepted")
	}
	if _, e = Verify(bytes.NewReader(JSONL([]Event{p[1], p[0]})), "", ""); e == nil {
		t.Fatal("reorder accepted")
	}
	if _, e = Open(dir, "test", 1<<20); e == nil {
		t.Fatal("second writer accepted")
	}
	s.Close()
	s, e = Open(dir, "test", 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if len(s.Pending()) != 2 {
		t.Fatal("lost durable events")
	}
	if e = s.Ack(2); e != nil {
		t.Fatal(e)
	}
	if e = s.Append([]Event{event()}); e != nil {
		t.Fatal(e)
	}
	if s.Pending()[0].PrevHash != p[1].Hash || s.Pending()[0].Sequence != 3 {
		t.Fatal("head lost after ack")
	}
}

type fakeSink struct {
	fail  bool
	calls int
}

func (s *fakeSink) Send(context.Context, []Event) error {
	s.calls++
	if s.fail {
		return errors.New("offline")
	}
	return nil
}
func TestAtLeastOnceAndCapacity(t *testing.T) {
	s := openTest(t)
	if e := s.Observe("uid", "rv", []Event{event()}); e != nil {
		t.Fatal(e)
	}
	if e := s.Observe("uid", "rv", []Event{event()}); e != nil {
		t.Fatal(e)
	}
	if len(s.Pending()) != 1 {
		t.Fatal("observation replay duplicated")
	}
	a, b := &fakeSink{}, &fakeSink{fail: true}
	if s.Flush(context.Background(), []Sink{a, b}) == nil {
		t.Fatal("failure ignored")
	}
	if len(s.Pending()) != 1 {
		t.Fatal("acked failed delivery")
	}
	b.fail = false
	if e := s.Flush(context.Background(), []Sink{a, b}); e != nil {
		t.Fatal(e)
	}
	if a.calls != 2 || len(s.Pending()) != 0 {
		t.Fatal("no replay")
	}
	s.max = 1024
	e := event()
	e.Details = map[string]any{"large": strings.Repeat("x", 2000)}
	if !errors.Is(s.Append([]Event{e}), ErrFull) {
		t.Fatal("capacity not enforced")
	}
	if len(s.Pending()) != 0 {
		t.Fatal("failed append mutated state")
	}
}
func TestReceiver(t *testing.T) {
	s := openTest(t)
	r := Receiver{Spool: s, Token: "token", Cluster: "mgmt"}
	body := `{"apiVersion":"audit.k8s.io/v1","kind":"EventList","items":[{"auditID":"id","stage":"ResponseComplete","verb":"patch","user":{"username":"alice"},"objectRef":{"apiGroup":"aep.aenix.io","resource":"organizations","name":"acme"},"requestObject":{"secret":"do-not-export"}},{"auditID":"ignore","stage":"ResponseComplete","objectRef":{"apiGroup":"other"}}]}`
	send := func(token string, b string) int {
		q := httptest.NewRequest("POST", "/kubernetes", strings.NewReader(b))
		q.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, q)
		return w.Code
	}
	if send("wrong", body) != 401 || send("token", "{}") == 204 || send("token", body) != 204 {
		t.Fatal("bad HTTP contract")
	}
	if len(s.Pending()) != 1 || bytes.Contains(JSONL(s.Pending()), []byte("do-not-export")) {
		t.Fatal("filter/redaction failed")
	}
	s.max = 1024
	if send("token", body) != 503 {
		t.Fatal("backpressure not surfaced")
	}
}
func TestOpenSearchFailure(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		ok     bool
	}{{500, `{}`, false}, {200, `{"errors":true,"items":[]}`, false}, {200, `{}`, false}, {200, `{"errors":false,"items":[{"index":{"status":201}}]}`, true}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			if !bytes.HasSuffix(b, []byte("\n")) {
				t.Error("missing NDJSON newline")
			}
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		}))
		err := (OpenSearch{URL: srv.URL, Index: "test"}).Send(context.Background(), []Event{event()})
		srv.Close()
		if (err == nil) != tc.ok {
			t.Fatalf("unexpected result %v", err)
		}
	}
}

type objectSink struct {
	keys []string
	fail bool
}

func (s *objectSink) Put(_ context.Context, k string, _ []byte, _ string) error {
	s.keys = append(s.keys, k)
	if s.fail {
		return errors.New("S3 offline")
	}
	return nil
}
func TestS3DailyImmutableKeys(t *testing.T) {
	s := openTest(t)
	a, b := event(), event()
	a.Time = "2026-11-25T23:59:59Z"
	b.Time = "2026-11-26T00:00:00Z"
	if e := s.Append([]Event{a, b}); e != nil {
		t.Fatal(e)
	}
	o := &objectSink{}
	sink := S3{o}
	pending := s.Pending()
	pending[0].RecordedAt = a.Time
	pending[1].RecordedAt = b.Time
	if e := sink.Send(context.Background(), pending); e != nil {
		t.Fatal(e)
	}
	if len(o.keys) != 2 || !strings.Contains(o.keys[0], "2026-11-25") {
		t.Fatal(o.keys)
	}
	first := o.keys[0]
	if e := sink.Send(context.Background(), pending); e != nil {
		t.Fatal(e)
	}
	if first != o.keys[2] {
		t.Fatal("retry changed key")
	}
	o.fail = true
	if sink.Send(context.Background(), pending) == nil {
		t.Fatal("S3 failure ignored")
	}
}
func TestSyslogTLSFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	err := (Syslog{Address: strings.TrimPrefix(srv.URL, "https://"), TLS: &tls.Config{MinVersion: tls.VersionTLS12}}).Send(context.Background(), []Event{event()})
	if err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}
func TestNormalizeChangeEvidence(t *testing.T) {
	var m map[string]any
	_ = json.Unmarshal([]byte(`{"kind":"ChangeRequest","metadata":{"uid":"u","name":"c","resourceVersion":"1","creationTimestamp":"2026-11-25T00:00:00Z"},"spec":{"requester":"alice","organization":"acme"},"status":{"phase":"Applied","approvals":[{"user":"bob","evidence":"CHG1"}],"notes":[{"text":"note"}],"before":[{"hash":"h"}]}}`), &m)
	events := Normalize(&unstructured.Unstructured{Object: m}, false)
	if len(events) != 4 || events[2].Actor != "bob" || events[3].Action != "changerequest.applied" || events[0].Details["beforeSnapshotRef"] == nil {
		t.Fatalf("bad lifecycle: %+v", events)
	}
}
func TestCorruptSpoolRejected(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Open(dir, "test", 1<<20); e == nil {
		t.Fatal("corruption ignored")
	}
}

func TestLargeIntegerChainAndUnknownField(t *testing.T) {
	s := openTest(t)
	e := event()
	e.Details = map[string]any{"counter": int64(9007199254740993)}
	if err := s.Append([]Event{e}); err != nil {
		t.Fatal(err)
	}
	b := JSONL(s.Pending())
	if _, err := Verify(bytes.NewReader(b), "", ""); err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte(`{"recordedAt"`), []byte(`{"unhashed":"injected","recordedAt"`), 1)
	if _, err := Verify(bytes.NewReader(b), "", ""); err == nil {
		t.Fatal("unknown field accepted")
	}
}
