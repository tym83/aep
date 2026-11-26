package itsm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
)

// Jira uses JSM request types for changes and Jira v3 for fields/transitions.
// Assets and Operations are separate, explicitly unsupported capabilities.
type Jira struct {
	HTTP                              HTTP
	Project, ServiceDesk, RequestType string
	Transitions                       map[string]string
}

func (j *Jira) Deliver(ctx context.Context, e Event, ref string) (string, error) {
	if e.Type == "alert.raised" || e.Type == "alert.resolved" {
		return j.alert(ctx, e, ref)
	}
	if e.Type != "change.created" && e.Type != "change.updated" {
		return ref, fmt.Errorf("Jira capability %s is not configured (Assets/Operations required)", e.Type)
	}
	var c Change
	if err := json.Unmarshal(e.Data, &c); err != nil {
		return ref, err
	}
	if c.Request == nil {
		return ref, errors.New("change required")
	}
	label := "aep-" + Digest([]byte(c.ID))
	if ref == "" {
		var result struct {
			Issues []struct {
				Key string `json:"key"`
			} `json:"issues"`
		}
		if !validID(j.Project) {
			return ref, errors.New("invalid Jira project")
		}
		q := url.QueryEscape(`project = "` + j.Project + `" AND labels = "` + label + `"`)
		if err := j.HTTP.Do(ctx, "GET", "/rest/api/3/search/jql?jql="+q+"&maxResults=2", "", nil, &result); err != nil {
			return ref, err
		}
		if len(result.Issues) > 1 {
			return ref, errors.New("duplicate Jira correlation label")
		}
		if len(result.Issues) == 1 {
			ref = result.Issues[0].Key
		}
	}
	summary := "AEP " + c.ID + " " + string(c.Request.Spec.Operation)
	if ref == "" {
		// Request type must expose labels; set it atomically with creation so a
		// timeout can be recovered without duplicating the request.
		fields := map[string]any{"summary": summary, "description": c.Request.Spec.Justification + "\nConfirm in AEP: " + c.ConfirmationURL, "labels": []string{label}}
		var out struct {
			Key string `json:"issueKey"`
		}
		if err := j.HTTP.Do(ctx, "POST", "/rest/servicedeskapi/request", e.ID, map[string]any{"serviceDeskId": j.ServiceDesk, "requestTypeId": j.RequestType, "requestFieldValues": fields}, &out); err != nil {
			return ref, err
		}
		ref = out.Key
	}
	if !validID(ref) {
		return ref, errors.New("invalid Jira issue key")
	}
	fields := map[string]any{"summary": summary, "description": map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]string{"type": "text", "text": string(e.Data)}}}}}}
	if err := j.HTTP.Do(ctx, "PUT", "/rest/api/3/issue/"+ref, e.ID, map[string]any{"fields": fields}, nil); err != nil {
		return ref, err
	}
	if transition := j.Transitions[string(c.Request.Status.Phase)]; transition != "" {
		// Only execute currently available transitions; an already completed
		// transition is absent and replay is a no-op.
		var available struct {
			Transitions []struct {
				ID string `json:"id"`
			} `json:"transitions"`
		}
		if err := j.HTTP.Do(ctx, "GET", "/rest/api/3/issue/"+ref+"/transitions", "", nil, &available); err != nil {
			return ref, err
		}
		for _, a := range available.Transitions {
			if a.ID == transition {
				return ref, j.HTTP.Do(ctx, "POST", "/rest/api/3/issue/"+ref+"/transitions", e.ID, map[string]any{"transition": map[string]string{"id": transition}}, nil)
			}
		}
	}
	return ref, nil
}

// Approvals returns vendor evidence; it never answers approvals as a bot.
func (j *Jira) Approvals(ctx context.Context, issue string) (json.RawMessage, error) {
	if !validID(issue) {
		return nil, errors.New("invalid issue key")
	}
	var out json.RawMessage
	err := j.HTTP.Do(ctx, "GET", "/rest/servicedeskapi/request/"+issue+"/approval", "", nil, &out)
	return out, err
}

func (j *Jira) alert(ctx context.Context, e Event, ref string) (string, error) {
	var a Alert
	if err := json.Unmarshal(e.Data, &a); err != nil {
		return ref, err
	}
	if a.Key == "" || !validID(j.Project) {
		return ref, errors.New("alert key and Jira project required")
	}
	label := "aep-alert-" + Digest([]byte(a.Key))
	if ref == "" {
		var result struct {
			Issues []struct {
				Key string `json:"key"`
			} `json:"issues"`
		}
		q := url.QueryEscape(`project = "` + j.Project + `" AND labels = "` + label + `"`)
		if err := j.HTTP.Do(ctx, "GET", "/rest/api/3/search/jql?jql="+q+"&maxResults=2", "", nil, &result); err != nil {
			return ref, err
		}
		if len(result.Issues) > 1 {
			return ref, errors.New("duplicate Jira alert correlation")
		}
		if len(result.Issues) == 1 {
			ref = result.Issues[0].Key
		}
	}
	fields := map[string]any{"summary": a.Summary, "description": map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]string{"type": "text", "text": string(e.Data)}}}}}}
	if ref == "" {
		if e.Type == "alert.resolved" {
			return "", nil
		}
		fields["project"] = map[string]string{"key": j.Project}
		fields["issuetype"] = map[string]string{"name": "Incident"}
		fields["labels"] = []string{label}
		var result struct {
			Key string `json:"key"`
		}
		if err := j.HTTP.Do(ctx, "POST", "/rest/api/3/issue", e.ID, map[string]any{"fields": fields}, &result); err != nil {
			return ref, err
		}
		ref = result.Key
	} else {
		if !validID(ref) {
			return ref, errors.New("invalid Jira issue key")
		}
		if err := j.HTTP.Do(ctx, "PUT", "/rest/api/3/issue/"+ref, e.ID, map[string]any{"fields": fields}, nil); err != nil {
			return ref, err
		}
	}
	if !validID(ref) {
		return ref, errors.New("Jira omitted issue key")
	}
	if transition := j.Transitions[e.Type]; transition != "" {
		var available struct {
			Transitions []struct {
				ID string `json:"id"`
			} `json:"transitions"`
		}
		if err := j.HTTP.Do(ctx, "GET", "/rest/api/3/issue/"+ref+"/transitions", "", nil, &available); err != nil {
			return ref, err
		}
		for _, item := range available.Transitions {
			if item.ID == transition {
				return ref, j.HTTP.Do(ctx, "POST", "/rest/api/3/issue/"+ref+"/transitions", e.ID, map[string]any{"transition": map[string]string{"id": transition}}, nil)
			}
		}
	}
	return ref, nil
}
