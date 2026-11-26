package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeycloakProjectGroups(t *testing.T) {
	s := integrationSession(t)
	ctx := context.Background()
	created := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"admin"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer admin" {
			t.Error("missing authorization")
		}
		var body map[string]string
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		created[body["name"]] = true
		w.WriteHeader(201)
	}))
	defer server.Close()
	s.r.Options.KeycloakURL = server.URL
	s.r.Options.KeycloakRealm = "acme"
	s.r.Options.KeycloakCredentialsSecret = "aep-system/git"
	for _, p := range []string{"payments", "shop"} {
		if e := s.cp.apply(ctx, s.o, "projects", obj(ocAPI, "Project", "acme", p, M{})); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.keycloak(ctx); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"aep-acme-payments-developer", "aep-acme-shop-developer", "aep-acme-developer", "aep-acme-platform"} {
		if !created[name] {
			t.Fatal("group missing", name)
		}
	}
}
