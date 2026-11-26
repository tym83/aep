package operator

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"strings"
	"testing"
	"time"
)

func integrationSession(t *testing.T) *session {
	t.Helper()
	o := argoOrg()
	o.UID = "org-uid"
	makeCluster := func() *Cluster {
		d := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvr(ocAPI, "components"): "ComponentList", gvr(ocAPI, "projects"): "ProjectList"})
		// Dynamic fake does not implement SSA; retain ownership checks in apply().
		d.PrependReactor("patch", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
			p := a.(ktesting.PatchAction)
			u := &unstructured.Unstructured{}
			if e := json.Unmarshal(p.GetPatch(), &u.Object); e != nil {
				return true, nil, e
			}
			e := d.Tracker().Update(p.GetResource(), u, p.GetNamespace())
			return true, u, e
		})
		return &Cluster{Dynamic: d, Kube: kubefake.NewSimpleClientset()}
	}
	s := &session{o: o, r: &Reconciler{Management: makeCluster(), Options: Options{Namespace: "aep-system"}, Recorder: record.NewFakeRecorder(30)}, cp: makeCluster(), tiers: map[string]*Cluster{"nonprod": makeCluster()}}
	for name, data := range map[string]map[string][]byte{"git": {"token": []byte("admin-token")}, "git-read": {"token": []byte("clone-token")}, "argocd-acme-oidc": {"clientSecret": []byte("oidc-secret"), "rootCA": []byte("CA")}} {
		_, err := s.r.Management.Kube.CoreV1().Secrets("aep-system").Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{OwnerLabel: "acme"}}, Data: data}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func dataValue(t *testing.T, u *unstructured.Unstructured, key string) string {
	t.Helper()
	v, _, _ := unstructured.NestedString(u.Object, "data", key)
	b, e := base64.StdEncoding.DecodeString(v)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func TestArgoCredentialsUseReadToken(t *testing.T) {
	s := integrationSession(t)
	ctx := context.Background()
	c := s.tiers["nonprod"]
	if e := s.argoCredentials(ctx, c); e != nil {
		t.Fatal(e)
	}
	repo, e := c.get(ctx, "v1", "secrets", "argocd", "aep-corporate-vcs")
	if e != nil {
		t.Fatal(e)
	}
	if dataValue(t, repo, "password") != "clone-token" || dataValue(t, repo, "url") != "https://git.example/acme/" || dataValue(t, repo, "username") != "oauth2" {
		t.Fatal("incorrect repository credentials")
	}
	b, _ := json.Marshal(repo)
	if strings.Contains(string(b), base64.StdEncoding.EncodeToString([]byte("admin-token"))) || strings.Contains(string(b), "githubAppPrivateKey") {
		t.Fatal("admin credential escaped")
	}
	oidc, e := c.get(ctx, "v1", "secrets", "argocd", "aep-oidc")
	if e != nil || dataValue(t, oidc, "clientSecret") != "oidc-secret" {
		t.Fatal(e)
	}
}
func TestVCSSecretOrganizationLabels(t *testing.T) {
	for _, name := range []string{"git", "git-read", "ca"} {
		t.Run(name, func(t *testing.T) {
			s := integrationSession(t)
			ctx := context.Background()
			if name == "ca" {
				s.o.Spec.VCS.CABundleSecretRef = &api.SecretNameReference{Name: "ca"}
				_, e := s.r.Management.Kube.CoreV1().Secrets("aep-system").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ca"}, Data: map[string][]byte{"ca.crt": []byte("CA")}}, metav1.CreateOptions{})
				if e != nil {
					t.Fatal(e)
				}
			}
			sec, e := s.r.Management.Kube.CoreV1().Secrets("aep-system").Get(ctx, name, metav1.GetOptions{})
			if e != nil {
				t.Fatal(e)
			}
			sec.Labels = map[string]string{OwnerLabel: "another-org"}
			s.r.Management.Kube.CoreV1().Secrets("aep-system").Update(ctx, sec, metav1.UpdateOptions{})
			if name == "ca" {
				_, e = s.vcsCA(ctx)
			} else {
				_, e = s.vcsNamedSecret(ctx, name)
			}
			if e == nil {
				t.Fatal("accepted foreign credentials")
			}
		})
	}
}
func TestIntegrationCleanup(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint("remove-vcs=", remove), func(t *testing.T) {
			s := integrationSession(t)
			ctx := context.Background()
			s.o.Spec.Tiers["nonprod"].ArgoCD.Enabled = false
			if remove {
				s.o.Spec.Tiers["nonprod"].ArgoCD.Enabled = true
				s.o.Spec.VCS = nil
				if e := validateIntegrations(s.o); e != nil {
					t.Fatal(e)
				}
			}
			artifacts := []struct {
				c                     *Cluster
				av, kind, r, ns, name string
			}{
				{s.tiers["nonprod"], "v1", "Secret", "secrets", "argocd", "aep-oidc"}, {s.tiers["nonprod"], "v1", "Secret", "secrets", "argocd", "aep-corporate-vcs"},
				{s.r.Management, helmAPI, "HelmRelease", "helmreleases", "tenant-acme-nonprod", "aep-argocd"},
				{s.r.Management, "source.toolkit.fluxcd.io/v1", "HelmRepository", "helmrepositories", "tenant-acme-nonprod", "aep-argocd"},
				{s.r.Management, "v1", "Secret", "secrets", "aep-system", "vcs-acme-build"}, {s.cp, ocAPI, "SecretReference", "secretreferences", "acme", "aep-corporate-vcs"},
			}
			for _, a := range artifacts {
				if e := a.c.apply(ctx, s.o, a.r, obj(a.av, a.kind, a.ns, a.name, nil)); e != nil {
					t.Fatal(e)
				}
			}
			for range 3 {
				_ = s.vcs(ctx)
				_ = s.argocd(ctx)
			}
			for _, a := range artifacts {
				if _, e := a.c.get(ctx, a.av, a.r, a.ns, a.name); !apierrors.IsNotFound(e) {
					t.Fatalf("retained %s: %v", a.name, e)
				}
			}
			if e := s.vcs(ctx); e != nil {
				t.Fatal(e)
			}
			if e := s.argocd(ctx); e != nil {
				t.Fatal(e)
			}
			if _, e := s.r.Management.Kube.CoreV1().Secrets("aep-system").Get(ctx, "git", metav1.GetOptions{}); e != nil {
				t.Fatal("removed external credentials")
			}
		})
	}
}
func TestVCSReconcileSkipsBadComponents(t *testing.T) {
	s := integrationSession(t)
	ctx := context.Background()
	hooks := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			t.Error("wrong API token")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/user"), strings.Contains(r.URL.Path, "/groups/"):
			fmt.Fprint(w, `{"id":1}`)
		case strings.HasSuffix(r.URL.Path, "/hooks"):
			if r.Method == "GET" {
				fmt.Fprint(w, `[]`)
			} else {
				hooks++
				w.WriteHeader(201)
			}
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	s.o.Spec.VCS.URL = server.URL
	s.o.Spec.VCS.CABundleSecretRef = &api.SecretNameReference{Name: "ca"}
	_, e := s.r.Management.Kube.CoreV1().Secrets("aep-system").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ca", Labels: map[string]string{OwnerLabel: "acme"}}, Data: map[string][]byte{"ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}}, metav1.CreateOptions{})
	if e != nil {
		t.Fatal(e)
	}
	s.r.Options.VCSWebhookURL = "https://cp.example/hooks"
	s.cp.Kube.CoreV1().Secrets("openchoreo-control-plane").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "git-webhook-secrets"}, Data: map[string][]byte{"gitlab-secret": []byte("hook-secret")}}, metav1.CreateOptions{})
	workflow := obj(ocAPI, "Workflow", "acme", "build", M{"parameters": M{"openAPIV3Schema": M{"properties": M{"repo": M{"type": "string", "x-openchoreo-component-parameter-repository-url": true}}}}})
	if e = s.cp.apply(ctx, s.o, "workflows", workflow); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"a-bad", "z-good"} {
		repo := server.URL + "/acme/repo"
		if name == "a-bad" {
			repo = "https://evil.example/repo"
		}
		if e = s.cp.apply(ctx, s.o, "components", obj(ocAPI, "Component", "acme", name, M{"autoBuild": true, "workflow": M{"kind": "Workflow", "name": "build", "parameters": M{"repo": repo}}})); e != nil {
			t.Fatal(e)
		}
	}
	// Missing WorkflowPlane must not prevent the valid component's hook.
	if e = s.vcs(ctx); e == nil || !strings.Contains(e.Error(), "WorkflowPlane") {
		t.Fatal(e)
	}
	if !s.vcsVerified || hooks != 1 {
		t.Fatal(s.vcsVerified, hooks)
	}
	build, e := s.r.Management.get(ctx, "v1", "secrets", "aep-system", "vcs-acme-build")
	if e != nil || dataValue(t, build, "password") != "clone-token" {
		t.Fatal(e)
	}
	if e = s.cp.apply(ctx, s.o, "workflowplanes", obj(ocAPI, "WorkflowPlane", "acme", "nonprod", M{"secretStoreRef": M{"name": "build-store"}})); e != nil {
		t.Fatal(e)
	}
	if e = s.vcs(ctx); e != nil {
		t.Fatal(e)
	}
	if hooks != 1 {
		t.Fatal("hooks reconciled before interval")
	}
	ref, e := s.cp.get(ctx, ocAPI, "secretreferences", "acme", "aep-corporate-vcs")
	if e != nil {
		t.Fatal(e)
	}
	data, _, _ := unstructured.NestedSlice(ref.Object, "spec", "data")
	if len(data) != 2 {
		t.Fatal(ref)
	}
	recorder := s.r.Recorder.(*record.FakeRecorder)
	found := false
	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, "a-bad") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing per-component event")
	}
}
func TestIntegrationErrorsDoNotGatePhase(t *testing.T) {
	scheme := runtime.NewScheme()
	if e := api.AddToScheme(scheme); e != nil {
		t.Fatal(e)
	}
	o := argoOrg()
	setCondition(o, "ControlPlaneHardened", true, "Verified", "OK")
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(o).WithObjects(o).Build()}
	_, e := r.runSteps(context.Background(), o, []reconcileStep{{"VCSReady", func(context.Context) error { return pending("missing WorkflowPlane") }}, {"ArgoCDReady", func(context.Context) error { return pending("VCS unavailable") }}})
	if e != nil || o.Status.Phase != "Ready" {
		t.Fatal(e, o.Status)
	}
	for _, c := range o.Status.Conditions {
		if (c.Type == "VCSReady" || c.Type == "ArgoCDReady") && c.Status != "False" {
			t.Fatal(c)
		}
	}
}
func TestWebhookInterval(t *testing.T) {
	r := &Reconciler{}
	if !r.claimHookReconcile("acme") || r.claimHookReconcile("acme") {
		t.Fatal("not throttled")
	}
	r.hookAttempts["acme"] = time.Now().Add(-16 * time.Minute)
	if !r.claimHookReconcile("acme") {
		t.Fatal("never retried")
	}
}

func TestArgoGitHubAppReceivesOnlyInstallationToken(t *testing.T) {
	s := integrationSession(t)
	ctx := context.Background()
	s.o.Spec.VCS.Provider = "github"
	s.o.Spec.VCS.Org = "acme"
	s.o.Spec.VCS.GroupPath = ""
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		if body["permissions"].(map[string]any)["contents"] != "read" {
			t.Error("not read-only")
		}
		fmt.Fprintf(w, `{"token":"installation-read","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	s.o.Spec.VCS.URL = server.URL
	s.o.Spec.VCS.CABundleSecretRef = &api.SecretNameReference{Name: "ca"}
	_, e = s.r.Management.Kube.CoreV1().Secrets("aep-system").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ca", Labels: map[string]string{OwnerLabel: "acme"}}, Data: map[string][]byte{"ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}}, metav1.CreateOptions{})
	if e != nil {
		t.Fatal(e)
	}
	secret, e := s.r.Management.Kube.CoreV1().Secrets("aep-system").Get(ctx, "git-read", metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	secret.Data = map[string][]byte{"githubAppID": []byte("1"), "githubAppInstallationID": []byte("2"), "githubAppPrivateKey": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})}
	if _, e = s.r.Management.Kube.CoreV1().Secrets("aep-system").Update(ctx, secret, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	c := s.tiers["nonprod"]
	if e = s.argoCredentials(ctx, c); e != nil {
		t.Fatal(e)
	}
	repo, e := c.get(ctx, "v1", "secrets", "argocd", "aep-corporate-vcs")
	if e != nil {
		t.Fatal(e)
	}
	if dataValue(t, repo, "password") != "installation-read" || dataValue(t, repo, "username") != "x-access-token" {
		t.Fatal("wrong installation credential")
	}
	data, _, _ := unstructured.NestedMap(repo.Object, "data")
	for key := range data {
		if strings.HasPrefix(key, "githubApp") {
			t.Fatal("private App credentials escaped", key)
		}
	}
}

func TestVCSSecretReferencePolicy(t *testing.T) {
	s := integrationSession(t)
	s.cp.Kube.(*kubefake.Clientset).PrependReactor("create", "selfsubjectreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectReview{Status: authv1.SelfSubjectReviewStatus{UserInfo: authv1.UserInfo{Username: "system:serviceaccount:aep-system:aep-operator"}}}, nil
	})
	if e := s.admission(context.Background()); e != nil {
		t.Fatal(e)
	}
	policy, e := s.cp.get(context.Background(), "admissionregistration.k8s.io/v1", "validatingadmissionpolicies", "", "aep-acme-vcs-secrets")
	if e != nil {
		t.Fatal(e)
	}
	runSecretReferenceAdmission(t, policy)
}
