//go:build e2e

package e2e

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

var configPath = flag.String("config", "", "Private YAML stand configuration")

type organization struct {
	Name  string
	Users map[string]string
}
type config struct {
	Endpoints     map[string]string
	Organizations []organization
	TokenCommand  []string
}
type action struct {
	Endpoint string
	Path     string
	Allow    []string
	Pending  string
}
type matrix struct {
	Roles   []string
	Actions map[string]action
}

func request(ctx context.Context, base, path, token string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return 0, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if len(body) > 4<<20 {
		return 0, "", fmt.Errorf("response exceeds limit")
	}
	return resp.StatusCode, string(body), err
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func TestMatrix(t *testing.T) {
	if *configPath == "" {
		t.Skip("stand config not supplied; no external requests")
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		t.Fatal(err)
	}
	var c config
	if err = yaml.UnmarshalStrict(raw, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Organizations) != 2 || c.Organizations[0].Name == c.Organizations[1].Name || len(c.TokenCommand) == 0 {
		t.Fatal("two distinct organizations and tokenCommand required")
	}
	raw, err = os.ReadFile("matrix.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var m matrix
	if err = yaml.UnmarshalStrict(raw, &m); err != nil {
		t.Fatal(err)
	}
	t.Log("role | action | target | expected | actual")
	for _, role := range m.Roles {
		token := ""
		if role != "anonymous" {
			source := 0
			lookup := role
			if role == "other" {
				source = 1
				lookup = "developer"
			}
			user := c.Organizations[source].Users[lookup]
			if user == "" {
				t.Fatalf("missing user for %s", role)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			args := append(append([]string{}, c.TokenCommand[1:]...), user)
			out, e := exec.CommandContext(ctx, c.TokenCommand[0], args...).Output()
			cancel()
			if e != nil {
				t.Fatalf("token provider failed for %s", role)
			}
			token = strings.TrimSpace(string(out))
			if token == "" || strings.ContainsAny(token, "\r\n") {
				t.Fatal("invalid token provider output")
			}
		}
		for name, a := range m.Actions {
			if a.Pending != "" {
				t.Logf("PENDING | %s | %s | %s", role, name, a.Pending)
				continue
			}
			base := c.Endpoints[a.Endpoint]
			if base == "" {
				t.Fatalf("missing endpoint %s", a.Endpoint)
			}
			for target, org := range c.Organizations {
				if role == "other" && target == 1 {
					continue
				}
				path := strings.ReplaceAll(a.Path, "{org}", org.Name)
				cross := target == 1 || role == "other"
				expected := 200
				if role == "anonymous" {
					expected = 401
				} else if cross || !contains(a.Allow, role) {
					expected = 403
				}
				code, body, e := request(context.Background(), base, path, token)
				if e != nil {
					t.Errorf("%s | %s | %d | transport failure", role, name, target)
					continue
				}
				ok := code == expected || (cross && role != "anonymous" && code == 404)
				if cross && strings.Contains(body, org.Name) {
					ok = false
				}
				if !ok {
					t.Errorf("%s | %s | %d | %d | %d (body redacted)", role, name, target, expected, code)
				}
			}
		}
	}
}
func TestRequest(t *testing.T) {
	var destinationCalled bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalled = true }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("token missing")
		}
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer origin.Close()
	code, _, err := request(context.Background(), origin.URL, "/", "fixture")
	if err != nil || code != 302 || destinationCalled {
		t.Fatalf("redirect isolation failed: %d %v", code, err)
	}
}
