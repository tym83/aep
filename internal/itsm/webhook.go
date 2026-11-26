package itsm

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func Signature(secret []byte, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

type Webhook struct {
	URL    string
	Secret []byte
	Client *http.Client
}

func (g *Webhook) Deliver(ctx context.Context, e Event, ref string) (string, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return ref, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", g.URL, bytes.NewReader(raw))
	if err != nil {
		return ref, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("X-AEP-Timestamp", ts)
	req.Header.Set("X-AEP-Signature", Signature(g.Secret, ts, raw))
	req.Header.Set("Idempotency-Key", e.ID)
	req.Header.Set("Content-Type", "application/cloudevents+json")
	c := g.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := c.Do(req)
	if err != nil {
		return ref, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ref, &HTTPError{resp.StatusCode}
	}
	return ref, nil
}

// Receiver accepts only organization-scoped notes/evidence, graphs and alerts.
// It cannot accept an approval command or a caller-supplied human token.
type Receiver struct {
	Engine       *Engine
	Secret       []byte
	Organization string
	Now          func() time.Time
}

func (s *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.URL.Path != "/v1/events" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<10))
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	ts := r.Header.Get("X-AEP-Timestamp")
	unix, err := strconv.ParseInt(ts, 10, 64)
	sig, decodeErr := hex.DecodeString(r.Header.Get("X-AEP-Signature"))
	expected, _ := hex.DecodeString(Signature(s.Secret, ts, body))
	if len(s.Secret) < 32 || err != nil || decodeErr != nil || now.Sub(time.Unix(unix, 0)) > 5*time.Minute || time.Unix(unix, 0).Sub(now) > 5*time.Minute || !hmac.Equal(sig, expected) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var event Event
	if json.Unmarshal(body, &event) != nil || event.Validate() != nil {
		http.Error(w, "invalid event", 400)
		return
	}
	switch event.Type {
	case "change.note":
		var n Note
		if json.Unmarshal(event.Data, &n) != nil || n.Organization != s.Organization || !validID(n.Change) || len(n.Text) > 3000 || n.Text == "" || (n.Decision != "" && n.Decision != "approve" && n.Decision != "reject") {
			http.Error(w, "invalid note", 400)
			return
		}
		event.Subject = "note/" + s.Organization + "/" + event.ID
	case "alert.raised", "alert.resolved":
		var a Alert
		if json.Unmarshal(event.Data, &a) != nil || a.Key == "" || len(a.Key) > 400 || a.Summary == "" || (a.Severity != "" && (a.Severity < "0" || a.Severity > "5" || len(a.Severity) != 1)) {
			http.Error(w, "invalid alert", 400)
			return
		}
		a.Key = s.Organization + "/" + a.Key
		event.Data, _ = json.Marshal(a)
		event.Subject = "alert/" + a.Key
	case "cmdb.sync", "ci.upserted", "ci.retired":
		var g Graph
		if json.Unmarshal(event.Data, &g) != nil || g.Organization != s.Organization {
			http.Error(w, "invalid graph", 400)
			return
		}
		if err := ValidateGraph(g); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		event.Subject = "cmdb/" + s.Organization
	default:
		http.Error(w, "this event type is not accepted inbound", 400)
		return
	}
	if err := s.Engine.Submit(r.Context(), event); err != nil {
		http.Error(w, "delivery pending; retry with the same event ID", 503)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Router sends notes through the neutral AEP contract using an organization
// service account. All vendor evidence remains text until a human confirms it.
type Router struct {
	Vendor       Adapter
	AEP          HTTP
	Organization string
}

func (s *Router) Deliver(ctx context.Context, e Event, ref string) (string, error) {
	if e.Type != "change.note" {
		return s.Vendor.Deliver(ctx, e, ref)
	}
	var n Note
	if err := json.Unmarshal(e.Data, &n); err != nil {
		return ref, err
	}
	if n.Organization != s.Organization || !validID(n.Change) {
		return ref, errors.New("note outside organization scope")
	}
	text := n.Text
	if n.ExternalID != "" || n.Decision != "" {
		evidence, _ := json.Marshal(map[string]string{"externalId": n.ExternalID, "approverId": n.ApproverID, "decision": n.Decision})
		text += "\nITSM evidence (human confirmation required): " + string(evidence)
	}
	if len(text) > 4000 {
		return ref, errors.New("note too long")
	}
	return ref, s.AEP.Do(ctx, "POST", "/v1/changes/"+s.Organization+"/"+n.Change+"/notes", e.ID, map[string]string{"text": strings.TrimSpace(text)}, nil)
}
