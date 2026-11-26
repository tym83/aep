package audit

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

func str(u *unstructured.Unstructured, path ...string) string {
	v, _, _ := unstructured.NestedString(u.Object, path...)
	return v
}
func val(u *unstructured.Unstructured, path ...string) any {
	v, _, _ := unstructured.NestedFieldCopy(u.Object, path...)
	return v
}

// Normalize retains status evidence, not arbitrary resource specs or Secrets.
func Normalize(u *unstructured.Unstructured, deleted bool) []Event {
	action := "changed"
	if deleted {
		action = "deleted"
	}
	base := Event{Source: "management-watch", Action: strings.ToLower(u.GetKind()) + "." + action, Actor: "unknown", Resource: u.GetKind() + "/" + u.GetNamespace() + "/" + u.GetName(), Details: map[string]any{"uid": string(u.GetUID()), "resourceVersion": u.GetResourceVersion(), "generation": u.GetGeneration()}}
	if u.GetKind() != "ChangeRequest" {
		base.Organization = u.GetName()
		base.Details["specHash"] = Digest(val(u, "spec"))
		return []Event{base}
	}
	base.Actor = str(u, "spec", "requester")
	base.Organization = str(u, "spec", "organization")
	base.Details["requester"] = base.Actor
	base.Details["approvals"] = val(u, "status", "approvals")
	base.Details["notes"] = val(u, "status", "notes")
	base.Details["phase"] = str(u, "status", "phase")
	base.Details["target"] = val(u, "spec", "target")
	for _, field := range []string{"operation", "model", "desired", "justification", "window", "externalRef", "requesterGroups"} {
		base.Details[field] = val(u, "spec", field)
	}
	base.Details["desiredHash"] = Digest(val(u, "spec", "desired"))
	base.Details["conditions"] = val(u, "status", "conditions")
	before := val(u, "status", "before")
	base.Details["beforeSnapshotRef"] = fmt.Sprintf("changerequest:%s@%s/status/before#sha256:%s", u.GetUID(), u.GetResourceVersion(), Digest(before))
	// Keep snapshots alongside their reference so deletion of the CR cannot erase evidence.
	base.Details["before"] = before
	if deleted {
		base.Action = "changerequest.deleted"
		return []Event{base}
	}
	base.Action = "changerequest.observed"
	out := []Event{base}
	created := base
	created.Action = "changerequest.created"
	created.Time = u.GetCreationTimestamp().UTC().Format(time.RFC3339Nano)
	created.ID = Digest([]any{u.GetUID(), "created"})
	out = append(out, created)
	approvals, _, _ := unstructured.NestedSlice(u.Object, "status", "approvals")
	for _, a := range approvals {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		e := base
		e.Action = "changerequest.decided"
		e.Actor, _ = m["user"].(string)
		e.ID = Digest([]any{u.GetUID(), "decision", a})
		e.Details = map[string]any{}
		for k, v := range base.Details {
			e.Details[k] = v
		}
		e.Details["decision"] = m
		out = append(out, e)
	}
	phase := str(u, "status", "phase")
	if phase == "Applied" || phase == "Failed" {
		e := base
		e.Action = "changerequest." + strings.ToLower(phase)
		e.ID = Digest([]any{u.GetUID(), phase})
		out = append(out, e)
	}
	return out
}

// Watch relists after watch expiry. The API audit webhook is the authoritative
// history for transitions compacted while the watcher is unavailable.
func Watch(ctx context.Context, c dynamic.Interface, s *Spool, resource string) {
	ri := c.Resource(schema.GroupVersionResource{Group: "aep.aenix.io", Version: "v1alpha1", Resource: resource})
	accept := func(u *unstructured.Unstructured, deleted, added bool) error {
		key := string(u.GetUID())
		rev := u.GetResourceVersion()
		if deleted {
			rev += "/deleted"
		}
		var checkpoint struct {
			Revision string
			IDs      []string
		}
		_ = json.Unmarshal([]byte(s.Seen(key)), &checkpoint)
		if checkpoint.Revision == rev {
			return nil
		}
		seen := map[string]bool{}
		for _, id := range checkpoint.IDs {
			seen[id] = true
		}
		normalized := Normalize(u, deleted)
		events := []Event{}
		for _, e := range normalized {
			if e.ID != "" && seen[e.ID] {
				continue
			}
			if e.ID != "" {
				checkpoint.IDs = append(checkpoint.IDs, e.ID)
			}
			if u.GetKind() == "Organization" && checkpoint.Revision == "" && !deleted {
				e.Action = organizationInitialAction(added)
			}
			events = append(events, e)
		}
		checkpoint.Revision = rev
		encoded, _ := json.Marshal(checkpoint)
		for ctx.Err() == nil {
			err := s.Observe(key, string(encoded), events)
			if err == nil {
				return nil
			}
			log.Printf("audit watch backpressure: %v", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		return ctx.Err()
	}
	for ctx.Err() == nil {
		list, err := ri.List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range list.Items {
				if err = accept(&list.Items[i], false, false); err != nil {
					return
				}
			}
			var w watch.Interface
			w, err = ri.Watch(ctx, metav1.ListOptions{ResourceVersion: list.GetResourceVersion(), AllowWatchBookmarks: true})
			if err == nil {
				for ev := range w.ResultChan() {
					if ev.Type == watch.Error {
						break
					}
					if ev.Type == watch.Bookmark {
						continue
					}
					u, ok := ev.Object.(*unstructured.Unstructured)
					if ok {
						if err = accept(u, ev.Type == watch.Deleted, ev.Type == watch.Added); err != nil {
							w.Stop()
							return
						}
					}
				}
				w.Stop()
			}
		}
		if err != nil {
			log.Printf("audit watch %s: %v", resource, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

type Credential struct {
	Token  string `json:"token"`
	Source string `json:"source"`
	Path   string `json:"path"`
}

func ValidateCredentials(credentials []Credential) error {
	if len(credentials) == 0 {
		return fmt.Errorf("per-source credentials required")
	}
	seen := map[string]bool{}
	for _, c := range credentials {
		if len(c.Token) < 32 || c.Source == "" || (c.Path != "/kubernetes" && c.Path != "/openchoreo") || seen[c.Token] {
			return fmt.Errorf("invalid or duplicate source credential")
		}
		seen[c.Token] = true
	}
	return nil
}

type Receiver struct {
	Spool       *Spool
	Credentials []Credential
	DeadLetters ObjectWriter
}

func (r Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	source := ""
	for _, credential := range r.Credentials {
		if credential.Token != "" && subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+credential.Token)) == 1 && req.URL.Path == credential.Path {
			source = credential.Source
		}
	}
	if source == "" {
		w.WriteHeader(401)
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 8<<20))
	if err != nil {
		http.Error(w, "body too large", 413)
		return
	}
	var events []Event
	var invalid []json.RawMessage
	switch req.URL.Path {
	case "/kubernetes":
		var list struct {
			APIVersion string            `json:"apiVersion"`
			Kind       string            `json:"kind"`
			Items      []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(b, &list) != nil || list.APIVersion != "audit.k8s.io/v1" || list.Kind != "EventList" {
			http.Error(w, "expected audit.k8s.io/v1 EventList", 400)
			return
		}
		for _, raw := range list.Items {
			var e struct {
				AuditID      string         `json:"auditID"`
				Stage        string         `json:"stage"`
				Verb         string         `json:"verb"`
				Time         string         `json:"stageTimestamp"`
				User         map[string]any `json:"user"`
				Impersonated map[string]any `json:"impersonatedUser"`
				Object       map[string]any `json:"objectRef"`
				Response     map[string]any `json:"responseStatus"`
				Annotations  map[string]any `json:"annotations"`
			}
			if json.Unmarshal(raw, &e) != nil || e.AuditID == "" || e.Stage == "" {
				invalid = append(invalid, raw)
				InvalidWebhookCount.Add(1)
				continue
			}
			if e.Time != "" {
				if _, err := time.Parse(time.RFC3339Nano, e.Time); err != nil {
					invalid = append(invalid, raw)
					InvalidWebhookCount.Add(1)
					continue
				}
			}
			group, _ := e.Object["apiGroup"].(string)
			if group != "aep.aenix.io" && group != "apps.cozystack.io" && group != "openchoreo.dev" && !strings.HasSuffix(group, ".openchoreo.dev") {
				continue
			}
			actor, _ := e.User["username"].(string)
			events = append(events, Event{ID: Digest([]string{source, e.AuditID, e.Stage}), Source: source, Time: e.Time, Action: e.Verb, Actor: actor, Resource: fmt.Sprint(e.Object["resource"], "/", e.Object["namespace"], "/", e.Object["name"]), Details: map[string]any{"auditID": e.AuditID, "stage": e.Stage, "user": e.User, "impersonatedUser": e.Impersonated, "objectRef": e.Object, "responseStatus": e.Response, "annotations": e.Annotations}})
		}
	case "/openchoreo":
		// Adapter for the documented structured audit record, not arbitrary pod logs.
		var e struct {
			ID       string         `json:"event_id"`
			Time     string         `json:"event_time"`
			Action   string         `json:"action"`
			Result   string         `json:"result"`
			Actor    map[string]any `json:"actor"`
			Resource map[string]any `json:"resource"`
			Request  string         `json:"request_id"`
		}
		if json.Unmarshal(b, &e) != nil || e.ID == "" || e.Action == "" || e.Result == "" {
			http.Error(w, "expected structured OpenChoreo audit record", 400)
			return
		}
		actor, _ := e.Actor["id"].(string)
		ns, _ := e.Resource["namespace"].(string)
		events = append(events, Event{ID: Digest([]string{source, e.ID}), Source: source, Time: e.Time, Action: e.Action, Actor: actor, Organization: ns, Resource: fmt.Sprint(e.Resource["type"], "/", e.Resource["name"]), Details: map[string]any{"actor": e.Actor, "resource": e.Resource, "result": e.Result, "requestID": e.Request}})
	default:
		w.WriteHeader(404)
		return
	}
	for _, e := range events {
		if e.Time != "" {
			if _, err = time.Parse(time.RFC3339Nano, e.Time); err != nil {
				http.Error(w, "invalid event timestamp", 400)
				return
			}
		}
	}
	if len(events) > 0 {
		if err = r.Spool.Append(events); err != nil {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "durable spool unavailable", 503)
			return
		}
	}
	for _, raw := range invalid {
		if err := deadLetter(req.Context(), r.DeadLetters, "webhook", map[string]any{"source": source, "item": raw}, "invalid_audit_event", "invalid identity or timestamp"); err != nil {
			http.Error(w, "dead-letter unavailable", 503)
			return
		}
	}
	w.WriteHeader(204)
}

func organizationInitialAction(added bool) string {
	if added {
		return "organization.created"
	}
	return "organization.observed"
}
