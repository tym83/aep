// Package itsm implements the versioned, vendor-neutral ITSM boundary.
package itsm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
)

// Event is a CloudEvents 1.0 structured event. Subject is the ordering and
// deduplication scope; ID identifies one immutable delivery.
type Event struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject"`
	Time            time.Time       `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
}

func NewEvent(kind, subject string, data any) Event {
	raw, _ := json.Marshal(data)
	return Event{SpecVersion: "1.0", ID: Digest([]byte(kind + subject + string(raw))), Source: "aep", Type: kind, Subject: subject, Time: time.Now().UTC(), DataContentType: "application/json", Data: raw}
}
func (e Event) Validate() error {
	if e.Time.IsZero() || e.SpecVersion != "1.0" || e.ID == "" || len(e.ID) > 255 || e.Source != "aep" || e.Subject == "" || len(e.Subject) > 512 || e.DataContentType != "application/json" || !json.Valid(e.Data) {
		return fmt.Errorf("invalid CloudEvent envelope")
	}
	switch e.Type {
	case "change.created", "change.updated", "cmdb.sync", "ci.upserted", "ci.retired", "alert.raised", "alert.resolved", "change.note":
	default:
		return fmt.Errorf("unsupported event type %q", e.Type)
	}
	return nil
}

type Change struct {
	ID              string             `json:"id"`
	Request         *api.ChangeRequest `json:"request"`
	ConfirmationURL string             `json:"confirmationUrl,omitempty"`
}
type CI struct {
	HostSysID    string `json:"hostSysId,omitempty"`
	K8sUID       string `json:"k8sUid,omitempty"`
	InstanceName string `json:"instanceName,omitempty"`
	FQDN         string `json:"fqdn,omitempty"`
	Port         string `json:"port,omitempty"`
	UID          string `json:"uid"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	ExternalID   string `json:"externalId,omitempty"`
	Retired      bool   `json:"retired,omitempty"`
}
type Relationship struct {
	Parent string `json:"parent"`
	Child  string `json:"child"`
	Type   string `json:"type"`
}
type Graph struct {
	Organization  string         `json:"organization"`
	Epoch         string         `json:"epoch"`
	Items         []CI           `json:"items"`
	Relationships []Relationship `json:"relationships"`
}
type Alert struct {
	Key      string `json:"key"`
	Node     string `json:"node"`
	Summary  string `json:"summary"`
	Severity string `json:"severity"`
}

// Approval evidence is not an authorization grant. Human decisions go directly
// to the change API with the human's token and reviewed resourceVersion.
type Note struct {
	Organization string `json:"organization"`
	Change       string `json:"change"`
	Text         string `json:"text"`
	ExternalID   string `json:"externalId,omitempty"`
	ApproverID   string `json:"approverId,omitempty"`
	Decision     string `json:"decision,omitempty"`
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func validID(s string) bool { return len(s) > 0 && len(s) <= 253 && safeID.MatchString(s) }

// ValidateGraph checks the neutral graph before any vendor translation.
func ValidateGraph(g Graph) error {
	if g.Organization == "" || g.Epoch == "" || len(g.Items) == 0 {
		return fmt.Errorf("graph organization, epoch and items are required")
	}
	seen := map[string]bool{}
	for _, ci := range g.Items {
		if ci.UID == "" || ci.Name == "" || seen[ci.UID] {
			return fmt.Errorf("invalid or duplicate CI UID")
		}
		seen[ci.UID] = true
		switch ci.Kind {
		case "organization", "application-service", "cluster", "database", "business-application", "technical-service", "service-offering":
		default:
			return fmt.Errorf("unsupported CI kind")
		}
		if ci.Kind == "business-application" && (ci.ExternalID == "" || ci.Retired) {
			return fmt.Errorf("Business Application must reference an existing customer-owned record")
		}
	}
	for _, rel := range g.Relationships {
		if !seen[rel.Parent] || !seen[rel.Child] || rel.Parent == rel.Child {
			return fmt.Errorf("invalid relationship endpoints")
		}
		switch rel.Type {
		case "Contains::Contained by", "Depends on::Used by", "Runs on::Runs", "Consumes::Consumed by":
		default:
			return fmt.Errorf("unsupported relationship")
		}
	}
	return nil
}

// ShortText is the only change representation allowed across the vendor boundary.
// Neither desired state, justification, notes nor credentials leave AEP.
func ShortText(c Change) string {
	phase := "unknown"
	if c.Request != nil {
		phase = string(c.Request.Status.Phase)
	}
	raw, _ := json.Marshal(c.Request)
	return "AEP result/phase: " + bounded(phase, 64) + "\nConfirm in AEP: " + bounded(c.ConfirmationURL, 1024) + "\nRevision: " + Digest(raw)[:12]
}
func bounded(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
