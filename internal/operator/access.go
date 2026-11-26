package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (s *session) access(ctx context.Context) error {
	if e := s.admission(ctx); e != nil {
		return e
	}
	for _, u := range RenderAuthz(s.o) {
		r := "authzroles"
		if u.GetKind() == "AuthzRoleBinding" {
			r = "authzrolebindings"
		}
		if e := s.cp.apply(ctx, s.o, r, u); e != nil {
			return e
		}
	}
	if !s.r.Options.KeycloakEnabled {
		return nil
	}
	return s.keycloak(ctx)
}
func (s *session) keycloak(ctx context.Context) error {
	opt := s.r.Options
	ns, name, ok := strings.Cut(opt.KeycloakCredentialsSecret, "/")
	if !ok {
		return fmt.Errorf("keycloak credentials require namespace/name")
	}
	secret, e := s.r.Management.Kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if e != nil {
		return e
	}
	base := strings.TrimRight(opt.KeycloakURL, "/")
	client := &http.Client{Timeout: 30 * time.Second}
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {string(secret.Data["username"])}, "password": {string(secret.Data["password"])}}
	req, e := http.NewRequestWithContext(ctx, "POST", base+"/realms/master/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, e := client.Do(req)
	if e != nil {
		return e
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	e = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token)
	resp.Body.Close()
	if resp.StatusCode != 200 || e != nil || token.AccessToken == "" {
		return fmt.Errorf("Keycloak authentication failed (HTTP %d)", resp.StatusCode)
	}
	groupRoles := append(append([]string{}, roles...), "finance")
	if argoEnabled(s.o) {
		projects, err := s.cp.list(ctx, ocAPI, "projects", s.o.Name)
		if err != nil {
			return err
		}
		for _, p := range projects {
			groupRoles = append(groupRoles, p.GetName()+"-developer")
		}
	}
	for _, role := range groupRoles {
		body, _ := json.Marshal(M{"name": "aep-" + s.o.Name + "-" + role})
		req, e = http.NewRequestWithContext(ctx, "POST", base+"/admin/realms/"+url.PathEscape(opt.KeycloakRealm)+"/groups", strings.NewReader(string(body)))
		if e != nil {
			return e
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		resp, e = client.Do(req)
		if e != nil {
			return e
		}
		resp.Body.Close()
		if resp.StatusCode != 201 && resp.StatusCode != 409 {
			return fmt.Errorf("Keycloak group %s: HTTP %d", role, resp.StatusCode)
		}
	}
	return nil
}
