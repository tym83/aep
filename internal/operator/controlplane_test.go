package operator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func controlPlane() *api.ControlPlane {
	enabled := true
	return &api.ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "cp-uid", Generation: 2}, Spec: api.ControlPlaneSpec{
		Tenant:  api.ControlPlaneTenant{Parent: "tenant-root", Name: "aepcp"},
		Cluster: api.ControlPlaneCluster{Name: "aepcp", Version: "v1.35", StorageClass: "replicated", NodeGroup: api.ControlPlaneNodeGroup{InstanceType: "u1.large", MinReplicas: 3, MaxReplicas: 4, DiskSize: "20Gi"}},
		Domain:  "cp.example.com",
		Ingress: api.ControlPlaneIngress{ClassName: "tenant-root", ClusterIssuer: "letsencrypt-prod"},
		OIDC: api.ControlPlaneOIDC{Issuer: "https://id.example.com/realms/aep", UsernameClaim: "preferred_username", CLIClientID: "openchoreo-cli",
			PortalClient: api.ControlPlaneClient{ClientID: "openchoreo-backstage-client", SecretRef: api.SecretKeyReference{Name: "aep-portal-client"}}},
		Portal:  api.ControlPlanePortal{Database: api.ControlPlaneDatabase{Size: "5Gi", Replicas: 1, ResourcesPreset: "small"}},
		Content: api.ControlPlaneContent{ComponentTypes: &enabled},
	}}
}

func defaultedSpec(t *testing.T, raw json.RawMessage) api.ControlPlaneSpec {
	t.Helper()
	var s api.ControlPlaneSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestControlPlaneNamespace(t *testing.T) {
	cp := controlPlane()
	if ns := ControlPlaneNamespace(cp); ns != "tenant-aepcp" {
		t.Fatal(ns)
	}
	cp.Spec.Tenant = api.ControlPlaneTenant{Parent: "tenant-aepdev", Name: "cp2"}
	if ns := ControlPlaneNamespace(cp); ns != "tenant-aepdev-cp2" {
		t.Fatal(ns)
	}
}

func TestValidateControlPlane(t *testing.T) {
	if err := ValidateControlPlane(controlPlane()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*api.ControlPlane)
		want   string
	}{
		{"long name", func(cp *api.ControlPlane) { cp.Name = strings.Repeat("a", 41) }, "name must be"},
		{"parent outside tenants", func(cp *api.ControlPlane) { cp.Spec.Tenant.Parent = "kube-system" }, "tenant.parent"},
		{"dashed tenant name", func(cp *api.ControlPlane) { cp.Spec.Tenant.Name = "aep-cp" }, "tenant.name must be"},
		{"namespace too long", func(cp *api.ControlPlane) {
			cp.Spec.Tenant = api.ControlPlaneTenant{Parent: "tenant-" + strings.Repeat("p", 40), Name: strings.Repeat("n", 30)}
		}, "not a valid namespace"},
		{"bad quota", func(cp *api.ControlPlane) { cp.Spec.Tenant.Quotas = api.Quotas{"cpu": "lots"} }, "tenant quota cpu"},
		{"replicas inverted", func(cp *api.ControlPlane) { cp.Spec.Cluster.NodeGroup.MaxReplicas = 2 }, "minReplicas <= maxReplicas"},
		{"bad disk", func(cp *api.ControlPlane) { cp.Spec.Cluster.NodeGroup.DiskSize = "0Gi" }, "diskSize"},
		{"single label domain", func(cp *api.ControlPlane) { cp.Spec.Domain = "localhost" }, "two labels"},
		{"long domain label", func(cp *api.ControlPlane) { cp.Spec.Domain = strings.Repeat("a", 64) + ".example.com" }, "valid host name"},
		{"both certificates", func(cp *api.ControlPlane) { cp.Spec.Ingress.TLSSecretRef = &api.SecretNameReference{Name: "tls"} }, "exactly one"},
		{"no certificate", func(cp *api.ControlPlane) { cp.Spec.Ingress.ClusterIssuer = "" }, "exactly one"},
		{"http issuer", func(cp *api.ControlPlane) { cp.Spec.OIDC.Issuer = "http://id.example.com" }, "oidc.issuer"},
		{"http token endpoint", func(cp *api.ControlPlane) { cp.Spec.OIDC.TokenURL = "http://id.example.com/token" }, "oidc.tokenURL"},
		{"no client secret", func(cp *api.ControlPlane) { cp.Spec.OIDC.PortalClient.SecretRef.Name = "" }, "portalClient.secretRef"},
		{"relative read API", func(cp *api.ControlPlane) { cp.Spec.Portal.ReadAPIURL = "/read" }, "portal.readAPIURL"},
		{"gateway host", func(cp *api.ControlPlane) { cp.Spec.ClusterGateway.Host = "gateway" }, "clusterGateway.host"},
		{"unknown resource type", func(cp *api.ControlPlane) { cp.Spec.Content.ResourceTypes = []string{"cozystack-mysql"} }, "unknown resource type"},
		{"bad publisher template", func(cp *api.ControlPlane) {
			cp.Spec.Content.Builds = &api.ControlPlaneBuilds{RegistryHost: "registry.example.com", PublisherClientIDTemplate: "{{.Organization"}
		}, "publisherClientIDTemplate"},
		{"bad forwarder", func(cp *api.ControlPlane) { cp.Spec.Audit = &api.ControlPlaneAudit{ForwarderService: "forwarder"} }, "audit.forwarderService"},
		{"adoption mismatch", func(cp *api.ControlPlane) {
			cp.Spec.Adoption = &api.ControlPlaneAdoption{Confirm: "tenant-aepdev-cp/aepcp"}
		}, "adoption.confirm must be tenant-aepcp/aepcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := controlPlane()
			tc.change(cp)
			if err := ValidateControlPlane(cp); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	cp := controlPlane()
	cp.Spec.Content.ResourceTypes = []string{"cozystack-redis", "cozystack-postgres"}
	cp.Spec.Adoption = &api.ControlPlaneAdoption{Confirm: "tenant-aepcp/aepcp"}
	cp.Spec.Ingress = api.ControlPlaneIngress{ClassName: "tenant-root", TLSSecretRef: &api.SecretNameReference{Name: "tls"}}
	if err := ValidateControlPlane(cp); err != nil {
		t.Fatal(err)
	}
}

func TestControlPlaneReconcilerRejectsAndSuspends(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	invalid, suspended := controlPlane(), controlPlane()
	invalid.Name, invalid.Spec.Ingress.ClusterIssuer = "broken", ""
	suspended.Name, suspended.Spec.Suspend = "drill", true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(invalid, suspended).WithStatusSubresource(&api.ControlPlane{}).Build()
	// No management cluster: neither a rejected nor a suspended ControlPlane may touch one.
	r := &ControlPlaneReconciler{Client: c}
	for _, tc := range []struct{ name, phase, reason string }{
		{"broken", "Rejected", "InvalidSpec"},
		{"drill", "Suspended", "Suspended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tc.name}}
			if res, err := r.Reconcile(context.Background(), req); err != nil || res.RequeueAfter != 0 {
				t.Fatal(res, err)
			}
			got := &api.ControlPlane{}
			if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
				t.Fatal(err)
			}
			ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
			if got.Status.Phase != tc.phase || ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != tc.reason || ready.ObservedGeneration != got.Generation || got.Status.ObservedGeneration != got.Generation {
				t.Fatalf("status %+v", got.Status)
			}
			if len(got.Status.Conditions) != 1 || got.Finalizers != nil {
				t.Fatalf("observe-only reconcile changed more than Ready: %+v %v", got.Status.Conditions, got.Finalizers)
			}
			// A second pass writes nothing.
			version := got.ResourceVersion
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), req.NamespacedName, got); err != nil || got.ResourceVersion != version {
				t.Fatalf("second reconcile rewrote status: %v", err)
			}
		})
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "missing"}}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateOrganizationControlPlaneRef(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		ref, accepted, annotation string
		reconciled                bool
		want                      string
	}{
		{name: "legacy new"},
		{name: "legacy reconciled", reconciled: true},
		{name: "new with reference", ref: "cp2"},
		{name: "accepted", ref: "cp2", accepted: "cp2", reconciled: true},
		{name: "late without annotation", ref: "cp2", reconciled: true, want: "requires annotation aep.aenix.io/adopt-control-plane=cp2"},
		{name: "late with other annotation", ref: "cp2", annotation: "cp3", reconciled: true, want: "requires annotation"},
		{name: "late adoption", ref: "cp2", annotation: "cp2", reconciled: true},
		{name: "changed", ref: "cp3", accepted: "cp2", annotation: "cp3", reconciled: true, want: "immutable"},
		{name: "removed", accepted: "cp2", reconciled: true, want: "immutable"},
		{name: "invalid", ref: "Not_Valid", want: "ControlPlane name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := organization()
			o.Spec.ControlPlaneRef, o.Status.ControlPlaneRef = tc.ref, tc.accepted
			if tc.annotation != "" {
				o.Annotations = map[string]string{api.AdoptControlPlaneAnnotation: tc.annotation}
			}
			if tc.reconciled {
				o.Finalizers = []string{finalizer}
			}
			err := Validate(o)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}
