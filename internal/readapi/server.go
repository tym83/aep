// Package readapi exposes organization-scoped portal views of management state.
package readapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/tym83/aep/internal/policy"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/template"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/authn"
	"github.com/tym83/aep/internal/bridge"
	"github.com/tym83/aep/internal/change"
	"github.com/tym83/aep/internal/cost"
	"github.com/tym83/aep/internal/kinds"
	"github.com/tym83/aep/internal/operator"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Server struct {
	complianceNamespace  string
	now                  func() time.Time
	Costs                cost.Reader
	client               client.Reader
	apps                 dynamic.Interface
	verifier             authn.Verifier
	orgNamespaceTemplate string
	console              *template.Template
	mux                  *http.ServeMux
	openChoreoURL        string
	openChoreo           *http.Client
}

func New(c client.Reader, apps dynamic.Interface, verifier authn.Verifier, orgNamespaceTemplate, consoleURLTemplate string, options ...func(*Server)) (*Server, error) {
	s := &Server{client: c, apps: apps, verifier: verifier, orgNamespaceTemplate: orgNamespaceTemplate, mux: http.NewServeMux()}
	s.complianceNamespace = "aep-system"
	for _, option := range options {
		option(s)
	}
	if consoleURLTemplate != "" {
		var err error
		s.console, err = template.New("console").Option("missingkey=error").Parse(consoleURLTemplate)
		if err != nil {
			return nil, fmt.Errorf("console URL template: %w", err)
		}
		if _, err = s.consoleURL("tenant-example-prod", "Postgres", "example"); err != nil {
			return nil, err
		}
	}
	s.mux.HandleFunc("GET /v1/organizations/{org}/costs", s.withAuth(s.costAccess))
	s.mux.HandleFunc("GET /v1/me", s.withAuth(s.me))
	s.mux.HandleFunc("GET /v1/organizations/{org}/services", s.withAuth(s.organization(s.services, []string{"developer", "platform", "approver", "admin", "auditor"})))
	s.mux.HandleFunc("GET /v1/organizations/{org}/changes", s.withAuth(s.organization(s.changes, []string{"approver", "auditor"})))
	s.mux.HandleFunc("GET /v1/organizations/{org}/compliance", s.withAuth(s.compliance))
	// Any member may ask; logs:view for the environment decides.
	s.mux.HandleFunc("GET /v1/organizations/{org}/projects/{project}/components/{component}/environments/{environment}/logs", s.withAuth(s.organization(s.logs, nil)))
	return s, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

type authenticated func(http.ResponseWriter, *http.Request, *authn.Identity)

func (s *Server) withAuth(next authenticated) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		token := authn.BearerToken(r.Header.Get("Authorization"))
		if token == "" {
			fail(w, 401, "a bearer token is required")
			return
		}
		id, err := s.verifier.Verify(r.Context(), token)
		if err != nil || id == nil {
			fail(w, 401, "the token is not valid")
			return
		}
		next(w, r, id)
	}
}

var roles = []string{"developer", "platform", "approver", "admin", "auditor", "finance"}

func memberships(groups []string) map[string][]string {
	return authn.Memberships(groups, roles)
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

type organizationView struct {
	ArgoCDURL     string   `json:"argocdUrl,omitempty"`
	RepositoryURL string   `json:"repositoryUrl,omitempty"`
	Name          string   `json:"name"`
	Roles         []string `json:"roles"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, id *authn.Identity) {
	groups := memberships(id.Groups)
	var orgs api.OrganizationList
	if err := s.client.List(r.Context(), &orgs); err != nil {
		fail(w, 500, "cannot list organizations")
		return
	}
	result := []organizationView{}
	for _, org := range orgs.Items {
		if rs := groups[org.Name]; len(rs) > 0 {
			view := organizationView{Name: org.Name, Roles: rs, ArgoCDURL: org.Status.ArgoCDURL}
			if org.Spec.VCS != nil {
				path := org.Spec.VCS.GroupPath
				if org.Spec.VCS.Provider != "gitlab" {
					path = org.Spec.VCS.Org
				}
				view.RepositoryURL = strings.TrimRight(org.Spec.VCS.URL, "/") + "/" + path
			}
			result = append(result, view)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	write(w, 200, struct {
		Username      string             `json:"username"`
		Organizations []organizationView `json:"organizations"`
	}{id.Username, result})
}
func (s *Server) organization(next func(http.ResponseWriter, *http.Request, *api.Organization), allowedRoles []string) authenticated {
	return func(w http.ResponseWriter, r *http.Request, id *authn.Identity) {
		org := r.PathValue("org")
		rs := memberships(id.Groups)[org]
		allowed := allowedRoles == nil && len(rs) > 0
		for _, role := range allowedRoles {
			allowed = allowed || contains(rs, role)
		}
		if !allowed {
			fail(w, 403, "organization access denied")
			return
		}
		object := &api.Organization{}
		if err := s.client.Get(r.Context(), client.ObjectKey{Name: org}, object); err != nil {
			if apierrors.IsNotFound(err) {
				fail(w, 404, "organization not found")
			} else {
				fail(w, 500, "cannot read organization")
			}
			return
		}
		next(w, r, object)
	}
}

type applicationView struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	ConsoleURL string `json:"consoleUrl,omitempty"`
}
type claimView struct {
	Name       string             `json:"name"`
	Generation int64              `json:"generation"`
	Conditions []metav1.Condition `json:"conditions"`
}
type serviceView struct {
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Labels      map[string]string `json:"labels"`
	Application applicationView   `json:"application"`
	Claim       claimView         `json:"claim"`
}

func (s *Server) services(w http.ResponseWriter, r *http.Request, org *api.Organization) {
	items := []serviceView{}
	resources := []struct {
		kind string
		gvr  schema.GroupVersionResource
	}{
		{"Postgres", kinds.Table["Postgres"].GVR()}, {"Redis", kinds.Table["Redis"].GVR()},
		{"Kubernetes", schema.GroupVersionResource{Group: "apps.cozystack.io", Version: "v1alpha1", Resource: "kuberneteses"}},
	}
	for _, tier := range []string{"nonprod", "prod"} {
		ns := operator.TenantNamespace(org.Name, tier)
		for _, resource := range resources {
			selector := bridge.Domain + "managed-by=bridge"
			if resource.kind == "Kubernetes" {
				if org.UID == "" {
					continue
				}
				selector = operator.OwnerLabel + "=" + string(org.UID)
			}
			apps, err := s.apps.Resource(resource.gvr).Namespace(ns).List(r.Context(), metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				fail(w, 500, "cannot list applications")
				return
			}
			for i := range apps.Items {
				app := &apps.Items[i]
				if resource.kind == "Kubernetes" && (app.GetName() != "apps" || app.GetLabels()[operator.OwnerLabel] != string(org.UID)) {
					continue
				}
				if resource.kind != "Kubernetes" && app.GetLabels()[bridge.Domain+"managed-by"] != "bridge" {
					continue
				}
				item, err := s.service(r.Context(), app, resource.kind)
				if err != nil {
					fail(w, 500, "cannot build console URL")
					return
				}
				items = append(items, item)
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	write(w, 200, struct {
		Items []serviceView `json:"items"`
	}{items})
}
func (s *Server) service(ctx context.Context, app *unstructured.Unstructured, kind string) (serviceView, error) {
	console, err := s.consoleURL(app.GetNamespace(), kind, app.GetName())
	item := serviceView{Name: app.GetNamespace() + "/" + kind + "/" + app.GetName(), Kind: "service", Labels: map[string]string{}, Application: applicationView{app.GetName(), app.GetNamespace(), console}}
	if kind == "Kubernetes" {
		item.Kind = "cluster"
	}
	for _, key := range []string{"organization", "tier", "project", "resource", "environment", "managed-by"} {
		if value := app.GetLabels()[bridge.Domain+key]; value != "" {
			item.Labels[bridge.Domain+key] = value
		}
	}
	ref := strings.Split(app.GetAnnotations()[bridge.Domain+"claim-ref"], "/")
	if len(ref) == 2 {
		item.Claim.Name = ref[1]
	}
	// Evaluate readiness like the bridge: the HelmRelease generation must be
	// observed and the live spec must match the spec the bridge applied.
	release := "kubernetes-" + app.GetName()
	if m, ok := kinds.Table[kind]; ok {
		release = m.Release(app.GetName())
	}
	hr, hrErr := s.apps.Resource(helmReleases).Namespace(app.GetNamespace()).Get(ctx, release, metav1.GetOptions{})
	if hrErr != nil {
		hr = nil
	}
	matches := true
	if kind != "Kubernetes" {
		spec, _, _ := unstructured.NestedMap(app.Object, "spec")
		matches = app.GetAnnotations()[bridge.Domain+"applied-spec-hash"] == policy.Hash(spec)
	}
	item.Claim.Generation = app.GetGeneration()
	item.Claim.Conditions = bridge.Readiness(app, hr, matches)
	return item, err
}

var helmReleases = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}

func (s *Server) consoleURL(namespace, kind, name string) (string, error) {
	if s.console == nil {
		return "", nil
	}
	var b bytes.Buffer
	if err := s.console.Execute(&b, struct{ Namespace, Kind, Name string }{namespace, kind, name}); err != nil {
		return "", err
	}
	u, err := url.Parse(b.String())
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("console URL must be an HTTP(S) URL without credentials")
	}
	return u.String(), nil
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request, org *api.Organization) {
	var list api.ChangeRequestList
	if err := s.client.List(r.Context(), &list, client.InNamespace(change.OrgNamespace(s.orgNamespaceTemplate, org.Name))); err != nil {
		fail(w, 500, "cannot list changes")
		return
	}
	items := []api.ChangeRequest{}
	for _, cr := range list.Items {
		if cr.Spec.Organization != org.Name {
			continue
		}
		// Project only the contract fields. Desired payloads, snapshots and arbitrary
		// annotations may contain sensitive application configuration.
		approvals := append([]api.ChangeApproval{}, cr.Status.Approvals...)
		items = append(items, api.ChangeRequest{
			TypeMeta:   metav1.TypeMeta{APIVersion: "aep.aenix.io/v1alpha1", Kind: "ChangeRequest"},
			ObjectMeta: metav1.ObjectMeta{Name: cr.Name, Namespace: cr.Namespace, ResourceVersion: cr.ResourceVersion},
			Spec:       api.ChangeRequestSpec{Organization: cr.Spec.Organization, Requester: cr.Spec.Requester, Operation: cr.Spec.Operation, Model: cr.Spec.Model, Target: cr.Spec.Target, Justification: cr.Spec.Justification, Window: cr.Spec.Window},
			Status:     api.ChangeRequestStatus{Phase: cr.Status.Phase, Approvals: approvals},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	write(w, 200, struct {
		Items []api.ChangeRequest `json:"items"`
	}{items})
}
func fail(w http.ResponseWriter, code int, message string) {
	write(w, code, map[string]string{"error": message})
}
func write(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
