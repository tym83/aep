package readapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/authn"
	"github.com/tym83/aep/internal/bridge"
	"github.com/tym83/aep/internal/operator"
	corev1 "k8s.io/api/core/v1"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fixture(t *testing.T, objects ...runtime.Object) (*Server, *dynamicfake.FakeDynamicClient, client.Client, func([]string, func(map[string]any)) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
		fmt.Fprintf(w, `{"keys":[{"kty":"RSA","alg":"RS256","use":"sig","kid":"test","n":%q,"e":%q}]}`, n, e)
	}))
	t.Cleanup(jwks.Close)
	verifier, err := authn.NewOIDCVerifier(context.Background(), "https://issuer.example", jwks.URL, "portal", "", "")
	if err != nil {
		t.Fatal(err)
	}
	sign := func(groups []string, mutate func(map[string]any)) string {
		claims := map[string]any{"iss": "https://issuer.example", "aud": "portal", "exp": time.Now().Add(time.Hour).Unix(), "sub": "alice-id", "preferred_username": "alice", "groups": groups}
		if mutate != nil {
			mutate(claims)
		}
		payload, _ := json.Marshal(claims)
		raw := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload)
		sum := sha256.Sum256([]byte(raw))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return raw + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&api.Organization{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: "org-uid"}}).Build()
	lists := map[schema.GroupVersionResource]string{}
	for resource, kind := range map[string]string{"postgreses": "PostgresList", "redises": "RedisList", "kuberneteses": "KubernetesList"} {
		lists[schema.GroupVersionResource{Group: "apps.cozystack.io", Version: "v1alpha1", Resource: resource}] = kind
	}
	d := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objects...)
	s, err := New(c, d, verifier, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return s, d, c, sign
}
func call(s *Server, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-User", "admin")
	r.Header.Set("X-Groups", "aep-acme-admin")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestIdentityAndAuthorization(t *testing.T) {
	s, _, _, sign := fixture(t)
	token := sign([]string{"/aep-acme-developer", "aep-acme-developer", "aep-acme-approver", "aep-missing-admin", "//aep-acme-admin", "aep-platform-team"}, nil)
	w := call(s, "/v1/me", token)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"username":"alice","organizations":[{"name":"acme","roles":["approver","developer"]}]}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/v1/me", "/v1/organizations/acme/services", "/v1/organizations/acme/changes", "/v1/organizations/acme/compliance"} {
		for _, token := range []string{"", "garbage", sign(nil, func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }), sign(nil, func(c map[string]any) { c["iss"] = "https://other.example" }), sign(nil, func(c map[string]any) { c["aud"] = "other" })} {
			if w := call(s, path, token); w.Code != 401 {
				t.Fatalf("%s: %d %s", path, w.Code, w.Body)
			}
		}
	}
	for _, tc := range []struct {
		path, group string
		code        int
	}{
		{"/v1/organizations/acme/services", "aep-other-admin", 403},
		{"/v1/organizations/acme/changes", "aep-acme-developer", 403},
		{"/v1/organizations/acme/changes", "aep-acme-admin", 403},
		{"/v1/organizations/acme/changes", "aep-acme-platform", 403},
		{"/v1/organizations/acme/changes", "/aep-acme-auditor", 200},
		{"/v1/organizations/acme/changes", "aep-acme-approver", 200},
		{"/v1/organizations/missing/services", "aep-missing-admin", 404},
		{"/v1/organizations/missing/services", "aep-acme-admin", 403},
	} {
		if w := call(s, tc.path, sign([]string{tc.group}, nil)); w.Code != tc.code {
			t.Errorf("%+v: %d %s", tc, w.Code, w.Body)
		}
	}
	if w := call(s, "/v1/me", sign(nil, nil)); !strings.Contains(w.Body.String(), `"organizations":[]`) {
		t.Fatal(w.Body)
	}
}
func app(kind, ns, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps.cozystack.io/v1alpha1", "kind": kind, "spec": map[string]any{"password": "do-not-return"}}}
	u.SetName(name)
	u.SetNamespace(ns)
	u.SetGeneration(3)
	u.SetLabels(labels)
	u.SetAnnotations(map[string]string{bridge.Domain + "claim-ref": "claims/orders", "password": "do-not-return"})
	u.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(3)}, map[string]any{"type": "WorkloadsReady", "status": "False", "observedGeneration": int64(3), "reason": "Failed", "message": "workloads failed"}}}
	return u
}
func TestServicesScopeAndProjection(t *testing.T) {
	managed := map[string]string{bridge.Domain + "managed-by": "bridge", bridge.Domain + "project": "payments", bridge.Domain + "resource": "orders", bridge.Domain + "environment": "prod", "password": "do-not-return"}
	s, d, _, sign := fixture(t,
		app("Postgres", "tenant-acme-prod", "db", managed), app("Redis", "tenant-acme-nonprod", "db", managed),
		app("Postgres", "tenant-other-prod", "foreign", managed), app("Postgres", "tenant-acme-prod", "unmanaged", nil),
		app("Kubernetes", "tenant-acme-prod", "apps", map[string]string{operator.OwnerLabel: "org-uid"}),
		app("Kubernetes", "tenant-acme-nonprod", "apps", map[string]string{operator.OwnerLabel: "other-uid"}),
		app("Kubernetes", "tenant-acme-prod", "other", map[string]string{operator.OwnerLabel: "org-uid"}),
	)
	s2, err := New(s.client, d, s.verifier, "", "https://console.example/{{.Namespace}}/{{.Kind}}/{{.Name}}")
	if err != nil {
		t.Fatal(err)
	}
	w := call(s2, "/v1/organizations/acme/services", sign([]string{"aep-acme-developer"}, nil))
	var out struct {
		Items []serviceView `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(out.Items) != 3 || strings.Contains(w.Body.String(), "do-not-return") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	names := map[string]bool{}
	for _, item := range out.Items {
		if names[item.Name] {
			t.Fatal("duplicate row name")
		}
		names[item.Name] = true
		if item.Claim.Name != "orders" || item.Claim.Generation != 3 || item.Claim.Conditions[1].Reason != "Updating" || item.Claim.Conditions[2].Status != metav1.ConditionTrue || item.Claim.Conditions[2].ObservedGeneration != 3 {
			t.Fatalf("%+v", item)
		}
		if !strings.HasPrefix(item.Application.ConsoleURL, "https://console.example/tenant-acme-") {
			t.Fatal(item)
		}
	}
	if out.Items[1].Kind != "cluster" {
		t.Fatal(out.Items)
	}
	w = call(s, "/v1/organizations/acme/services", sign([]string{"aep-acme-developer"}, nil))
	if strings.Contains(w.Body.String(), "consoleUrl") {
		t.Fatal(w.Body)
	}
	for _, action := range d.Actions() {
		get := action.GetVerb() == "get" && action.GetResource().Resource == "helmreleases"
		if (action.GetVerb() != "list" && !get) || !strings.HasPrefix(action.GetNamespace(), "tenant-acme-") {
			t.Fatalf("unexpected action: %+v", action)
		}
	}
	d.PrependReactor("list", "redises", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("backend unavailable: sensitive detail")
	})
	w = call(s, "/v1/organizations/acme/services", sign([]string{"aep-acme-developer"}, nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "sensitive") {
		t.Fatal(w)
	}
}
func TestReadinessPreservesStaleObservations(t *testing.T) {
	s, _, _, _ := fixture(t)
	u := app("Postgres", "tenant-acme-prod", "db", nil)
	u.SetGeneration(4)
	item, err := s.service(context.Background(), u, "Postgres")
	if err != nil {
		t.Fatal(err)
	}
	if item.Claim.Conditions[2].ObservedGeneration != 3 || item.Claim.Generation != 4 {
		t.Fatal(item)
	}
	delete(u.Object, "status")
	item, _ = s.service(context.Background(), u, "Postgres")
	if item.Claim.Conditions[1].Status == metav1.ConditionTrue || item.Claim.Conditions[2].Status == metav1.ConditionTrue {
		t.Fatal(item)
	}
}
func TestChangesScopeAndProjection(t *testing.T) {
	s, _, c, sign := fixture(t)
	s.orgNamespaceTemplate = "changes-{org}"
	for _, tc := range []struct{ ns, name, org string }{{"changes-acme", "yes", "acme"}, {"changes-acme", "wrong-spec", "other"}, {"tenant-acme", "wrong-namespace", "acme"}, {"changes-other", "foreign", "other"}} {
		cr := &api.ChangeRequest{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: tc.ns, Annotations: map[string]string{"sensitive": "do-not-return"}}, Spec: api.ChangeRequestSpec{Organization: tc.org, Requester: "alice", Operation: api.OperationPromote, Justification: "ship", Desired: &ext.JSON{Raw: []byte(`{"password":"do-not-return"}`)}}, Status: api.ChangeRequestStatus{Phase: api.ChangePhase("Pending"), Approvals: []api.ChangeApproval{{User: "bob", Decision: "approve"}}}}
		if err := c.Create(context.Background(), cr); err != nil {
			t.Fatal(err)
		}
	}
	w := call(s, "/v1/organizations/acme/changes", sign([]string{"aep-acme-auditor"}, nil))
	var out struct {
		Items []api.ChangeRequest `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(out.Items) != 1 || out.Items[0].Name != "yes" || out.Items[0].ResourceVersion == "" || len(out.Items[0].Status.Approvals) != 1 || strings.Contains(w.Body.String(), "do-not-return") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
func TestConsoleTemplateValidation(t *testing.T) {
	for _, value := range []string{"{{", "javascript:alert(1)", "https://user:password@example.org", "https://example.org/{{.Unknown}}"} {
		if _, err := New(nil, nil, nil, "", value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestMembershipGroupBoundaries(t *testing.T) {
	groups := []string{"aep-admin", "aep-developer", "aep--approver", "aep-acme-owner", "aep-UPPER-admin", "/parent/aep-acme-admin", "//aep-acme-admin", "aep-acme-admin/"}
	if got := memberships(groups); len(got) != 0 {
		t.Fatalf("malformed groups granted access: %v", got)
	}
	for _, role := range roles {
		groups = append(groups, "aep-team-one-"+role, "/aep-team-one-"+role)
	}
	got := memberships(groups)
	if len(got) != 1 || len(got["team-one"]) != 5 {
		t.Fatalf("valid roles or hyphenated organization lost: %v", got)
	}
}
