//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

var configPath = flag.String("config", "", "Private YAML acceptance configuration; omitted means local tests only")

type organization struct {
	Name                       string
	Users                      map[string]string
	TierKubeconfig             string
	ProbeNamespace             string
	PostgresAddress            string
	APIAddress                 string
	LeakMarkers                []string
	LogsProject, LogsComponent string
}
type config struct {
	Endpoints                        map[string]string
	Organizations                    []organization
	TokenCommand, PortalTokenCommand []string
	Cleanup                          struct {
		TokenCommand    []string
		AepcpKubeconfig string
	}
	ManagementKubeconfig string
	Resources            struct {
		Project, ResourceType, Environment, ProdEnvironment string
		Parameters                                          map[string]any
		ComponentSpec, SecretSpec                           map[string]any
		ProtectedSpecs                                      map[string]map[string]any
		LogsPath, OutputsPath                               string
		OutputFields                                        []string
	}
	Network        struct{ Image, ControlAddress string }
	TimeoutSeconds int
}
type action struct {
	Endpoint, Path, Method, Fixture string
	Allow                           []string
	Success                         []int
	Body                            map[string]any
}
type matrix struct {
	Roles   []string
	Actions map[string]action
}
type suite struct {
	t          *testing.T
	c          config
	run        string
	tokens     map[string]string
	tokenTimes map[string]time.Time
	fixtures   []fixture
}
type fixture struct {
	project, resource, component, projectRelease, resourceRelease string
	change                                                        string
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func command(command []string, args ...string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("command not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, command[0], append(append([]string{}, command[1:]...), args...)...).Output()
	if err != nil {
		return "", fmt.Errorf("command failed (output redacted)")
	}
	return strings.TrimSpace(string(out)), nil
}
func token(commandLine []string, user string) (string, error) {
	out, err := command(commandLine, user)
	if err != nil {
		return "", err
	}
	if out == "" || strings.ContainsAny(out, "\r\n") {
		return "", fmt.Errorf("invalid token output")
	}
	return out, nil
}
func exchange(ctx context.Context, base, path, method, tok string, body any, headers map[string]string) (int, string, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		reader = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, reader)
	if err != nil {
		return 0, "", err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("HTTP transport failed (details redacted)")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if len(b) > 4<<20 {
		return 0, "", fmt.Errorf("response exceeds limit")
	}
	return resp.StatusCode, string(b), err
}
func (s *suite) auth(org int, role string, portal bool) string {
	if role == "anonymous" {
		return ""
	}
	if role == "other" {
		org = 1
		role = "developer"
	}
	key := fmt.Sprintf("%d/%s/%t", org, role, portal)
	if v := s.tokens[key]; v != "" && (s.tokenTimes == nil || time.Since(s.tokenTimes[key]) < 30*time.Second) {
		return v
	}
	user := s.c.Organizations[org].Users[role]
	if user == "" {
		s.t.Fatalf("missing identity %d/%s", org, role)
	}
	cmd := s.c.TokenCommand
	if portal {
		cmd = s.c.PortalTokenCommand
	}
	v, err := token(cmd, user)
	if err != nil {
		s.t.Fatal(err)
	}
	s.tokens[key] = v
	if s.tokenTimes != nil {
		s.tokenTimes[key] = time.Now()
	}
	return v
}
func (s *suite) call(org int, role, endpoint, method, path string, body any, headers map[string]string) (int, string) {
	s.t.Helper()
	tok := s.auth(org, role, false)
	if endpoint == "portal" {
		if headers == nil {
			headers = map[string]string{}
		}
		headers["x-openchoreo-token"] = tok
		tok = s.auth(org, role, true)
		path = "/api/aep" + path
	}
	code, raw, err := exchange(context.Background(), s.c.Endpoints[endpoint], path, method, tok, body, headers)
	if err != nil {
		s.t.Fatal(err)
	}
	return code, raw
}
func (s *suite) require(org int, role, endpoint, method, path string, body any, headers map[string]string, codes ...int) map[string]any {
	s.t.Helper()
	code, raw := s.call(org, role, endpoint, method, path, body, headers)
	ok := false
	for _, want := range codes {
		ok = ok || code == want
	}
	if !ok {
		s.t.Fatalf("%s %s %s: HTTP %d, want %v (body redacted)", role, method, endpoint, code, codes)
	}
	out := map[string]any{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}
func field(v map[string]any, path string) any {
	var cur any = v
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}
func str(v map[string]any, path string) string { x, _ := field(v, path).(string); return x }
func (s *suite) wait(label string, check func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(time.Duration(s.c.TimeoutSeconds) * time.Second)
	for {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timeout: %s", label)
		}
		time.Sleep(2 * time.Second)
	}
}
func (s *suite) name(prefix string) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		s.t.Fatal(err)
	}
	return fmt.Sprintf("%s-%s-%x", prefix, s.run, b)
}
func (s *suite) oc(org int, path string) string {
	return "/api/v1/namespaces/" + s.c.Organizations[org].Name + path
}
func object(name string, spec map[string]any, run string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{"aep.aenix.io/e2e-run": run}}, "spec": spec}
}
func loadConfig(path string) (config, error) {
	var c config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = yaml.UnmarshalStrict(b, &c); err != nil {
		return c, err
	}
	if len(c.Organizations) != 2 || c.Organizations[0].Name == c.Organizations[1].Name {
		return c, fmt.Errorf("two distinct organizations required")
	}
	if c.Organizations[0].Users["developer"] == c.Organizations[0].Users["approver"] || c.Organizations[1].Users["developer"] == c.Organizations[1].Users["approver"] || c.Organizations[0].Users["breakglass-requester"] == c.Organizations[0].Users["security-oncall"] {
		return c, fmt.Errorf("requesters and approvers must be distinct people")
	}
	dns := regexp.MustCompile(`^[a-z][a-z0-9-]{0,35}$`)
	for _, o := range c.Organizations {
		if !dns.MatchString(o.Name) {
			return c, fmt.Errorf("invalid organization name")
		}
		if o.Users["developer"] == "" || o.Users["approver"] == "" {
			return c, fmt.Errorf("developer and approver identities required in both organizations")
		}
	}
	for _, role := range []string{"platform", "approver", "admin", "auditor", "finance", "breakglass", "security-oncall", "breakglass-requester"} {
		if c.Organizations[0].Users[role] == "" {
			return c, fmt.Errorf("missing %s identity", role)
		}
	}
	for _, key := range []string{"read", "change", "openchoreo", "portal"} {
		u, err := url.Parse(c.Endpoints[key])
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return c, fmt.Errorf("invalid %s endpoint", key)
		}
	}
	if len(c.TokenCommand) == 0 || len(c.PortalTokenCommand) == 0 {
		return c, fmt.Errorf("tokenCommand and portalTokenCommand required")
	}
	if len(c.Cleanup.TokenCommand) == 0 && c.Cleanup.AepcpKubeconfig == "" {
		return c, fmt.Errorf("cleanup identity or aep-cp kubeconfig required")
	}
	r := c.Resources
	if !dns.MatchString(r.Project) || r.ResourceType == "" || r.Environment == "" || r.ProdEnvironment == "" || r.Environment == r.ProdEnvironment || len(r.ComponentSpec) == 0 || len(r.SecretSpec) == 0 || r.LogsPath == "" || r.OutputsPath == "" || len(r.OutputFields) == 0 {
		return c, fmt.Errorf("complete resource fixtures, log/output routes and distinct environments required")
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 900
	}
	if c.TimeoutSeconds < 1 {
		return c, fmt.Errorf("positive timeout required")
	}
	return c, nil
}
func TestAcceptance(t *testing.T) {
	if *configPath == "" {
		t.Skip("no config supplied; local tests make no stand requests")
	}
	c, err := loadConfig(*configPath)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	if _, err = rand.Read(b); err != nil {
		t.Fatal(err)
	}
	s := &suite{t: t, c: c, run: hex.EncodeToString(b), tokens: map[string]string{}, tokenTimes: map[string]time.Time{}, fixtures: make([]fixture, 2)}
	t.Logf("run id: %s", s.run)
	for i := range c.Organizations {
		s.setup(i)
	}
	t.Run("matrix", func(t *testing.T) { child := *s; child.t = t; child.matrix() })
	t.Run("journeys", func(t *testing.T) { child := *s; child.t = t; child.journeys() })
	t.Run("network-and-kubectl", func(t *testing.T) { child := *s; child.t = t; child.isolation() })
}
func sortedActions(m matrix) []string {
	keys := make([]string, 0, len(m.Actions))
	for k := range m.Actions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
