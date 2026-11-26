package itsm

import (
	"context"
	"encoding/json"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestJiraAmbiguousCreateWaitsAcrossRestart(t *testing.T) {
	now := time.Now()
	creates := 0
	indexed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "search"):
			if r.URL.Query().Get("fields") != "key" || !strings.Contains(r.URL.Query().Get("jql"), "ORDER BY created ASC") {
				t.Error("unbounded or unordered search")
			}
			if indexed {
				fmt.Fprint(w, `{"issues":[{"key":"OPS-1"},{"key":"OPS-2"}]}`)
			} else {
				fmt.Fprint(w, `{"issues":[]}`)
			}
		case r.Method == "POST":
			creates++
			http.Error(w, "ambiguous", 503)
		case r.Method == "PUT":
			var body struct{ Fields map[string]any }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Fields["description"] != nil {
				t.Error("description overwritten")
			}
			w.WriteHeader(204)
		default:
			t.Error(r.URL)
		}
	}))
	defer srv.Close()
	makeJira := func() *Jira {
		return &Jira{HTTP: HTTP{BaseURL: srv.URL}, Project: "OPS", Now: func() time.Time { return now }}
	}
	ref, err := makeJira().Deliver(context.Background(), changeEvent(api.PhaseApplied), "")
	if err == nil || !strings.HasPrefix(ref, jiraPending) {
		t.Fatal(ref, err)
	}
	now = now.Add(4 * time.Minute)
	next, err := makeJira().Deliver(context.Background(), changeEvent(api.PhaseApplied), ref)
	if err == nil || next != ref || creates != 1 {
		t.Fatal("recreated before index deadline")
	}
	indexed = true
	next, err = makeJira().Deliver(context.Background(), changeEvent(api.PhaseApplied), ref)
	if err != nil || next != "OPS-1" || creates != 1 {
		t.Fatal(next, err)
	}
	indexed = false
	now = now.Add(2 * time.Minute)
	_, _ = makeJira().Deliver(context.Background(), changeEvent(api.PhaseApplied), ref)
	if creates != 2 {
		t.Fatal("bounded deadline never expired")
	}
}
