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
	KeyID  string
	Client *http.Client
}

func (g *Webhook) Deliver(ctx context.Context, e Event, ref string) (string, error) {
	if e.Type == "change.created" || e.Type == "change.updated" {
		var c Change
		if err := json.Unmarshal(e.Data, &c); err != nil {
			return ref, err
		}
		e.Data, _ = json.Marshal(map[string]string{"id": bounded(c.ID, 512), "text": ShortText(c)})
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return ref, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", g.URL, bytes.NewReader(raw))
	if err != nil {
		return ref, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("X-AEP-Key-ID", g.KeyID)
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

// Receiver accepts only organization-scoped notes/evidence and trusted-sender alerts.
// It cannot accept an approval command or a caller-supplied human token.
type Receiver struct {
	Engine       *Engine
	NoteEngine   *Engine
	Secret       []byte
	Keys         map[string][]byte
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
	secret := s.Secret
	if s.Keys != nil {
		secret = s.Keys[r.Header.Get("X-AEP-Key-ID")]
	}
	expected, _ := hex.DecodeString(Signature(secret, ts, body))
	if len(secret) < 32 || err != nil || decodeErr != nil || now.Sub(time.Unix(unix, 0)) > 5*time.Minute || time.Unix(unix, 0).Sub(now) > 5*time.Minute || !hmac.Equal(sig, expected) {
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
		if json.Unmarshal(event.Data, &n) != nil || n.Organization != s.Organization || !validID(n.Change) || validateNote(n) != nil || (n.Decision != "" && n.Decision != "approve" && n.Decision != "reject") {
			http.Error(w, "invalid note", 400)
			return
		}
		event.Subject = "note/" + s.Organization + "/" + event.ID
	case "alert.raised", "alert.resolved":
		var a Alert
		if json.Unmarshal(event.Data, &a) != nil || a.Key == "" || len(a.Key) > 400 || a.Summary == "" || len(a.Summary) > 3000 || len(a.Node) > 255 || (a.Severity != "" && (a.Severity < "0" || a.Severity > "5" || len(a.Severity) != 1)) {
			http.Error(w, "invalid alert", 400)
			return
		}
		a.Key = s.Organization + "/" + a.Key
		event.Data, _ = json.Marshal(a)
		event.Subject = "alert/" + a.Key

	default:
		http.Error(w, "this event type is not accepted inbound", 400)
		return
	}
	engine := s.Engine
	if event.Type == "change.note" && s.NoteEngine != nil {
		engine = s.NoteEngine
	}
	if err := engine.Submit(r.Context(), event); err != nil {
		if status := permanent(err); status != 0 {
			http.Error(w, "delivery permanently rejected", status)
			return
		}
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
	if err := validateNote(n); err != nil {
		return ref, &HTTPError{400}
	}
	text := noteText(n)
	return ref, s.AEP.Do(ctx, "POST", "/v1/changes/"+s.Organization+"/"+n.Change+"/notes", e.ID, map[string]string{"text": strings.TrimSpace(text)}, nil)
}

func noteText(n Note) string {
	text := n.Text
	if n.ExternalID != "" || n.Decision != "" || n.ApproverID != "" {
		evidence, _ := json.Marshal(map[string]string{"externalId": n.ExternalID, "approverId": n.ApproverID, "decision": n.Decision})
		text += "\nITSM evidence reference (not confirmation): " + string(evidence)
	}
	return text
}
func validateNote(n Note) error {
	if strings.TrimSpace(n.Text) == "" || len(n.Text) > 3000 || len(n.ExternalID) > 255 || len(n.ApproverID) > 255 || len(n.Decision) > 16 || len(noteText(n)) > 4000 {
		return errors.New("invalid note length")
	}
	return nil
}
