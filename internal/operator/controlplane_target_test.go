package operator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	api "github.com/tym83/aep/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// servingControlPlane is a ControlPlane that finished the steps
// Organizations wait for.
func servingControlPlane(name string) *api.ControlPlane {
	cp := controlPlane()
	cp.Name, cp.UID = name, types.UID(name+"-uid")
	cp.Spec.Tenant.Name = name
	for _, t := range controlPlaneReadiness {
		setControlPlaneCondition(cp, t, true, "Reconciled", "Desired state verified")
	}
	cp.Status.Outputs = api.ControlPlaneOutputs{KubeconfigSecretRef: &api.SecretNameReference{Name: "operator-access-cp-" + name}, TenantNamespace: ControlPlaneNamespace(cp), ClusterName: "aepcp"}
	cp.Status.Gateway = api.ControlPlaneGatewayStatus{Host: "cluster-gateway." + name + ".aep.internal", URL: "wss://cluster-gateway." + name + ".aep.internal:8443/ws", MirrorService: ControlPlaneNamespace(cp) + "/kubernetes-aepcp-cluster-gateway-external", TargetPort: 30268}
	return cp
}

func organizationReconciler(t *testing.T, opts Options, objects ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&api.Organization{}, &api.ControlPlane{}).Build()
	return &Reconciler{Client: c, Options: opts}
}

func TestControlPlaneForPrecedence(t *testing.T) {
	ctx := context.Background()
	legacy := Options{CPSecret: "aep-system/aepcp", CPNamespace: "tenant-aep-cp", CPCluster: "aepcp"}
	withDefault := Options{DefaultControlPlane: "default", CPNamespace: "tenant-aep-cp", CPCluster: "aepcp"}
	both := legacy
	both.DefaultControlPlane = "default"
	for _, tc := range []struct {
		name         string
		opts         Options
		spec, status string
		want         string // ControlPlane name, "" for legacy
		missing      bool
	}{
		{name: "reference", opts: legacy, spec: "cp2", want: "cp2"},
		{name: "recorded reference", opts: withDefault, status: "cp2", want: "cp2"},
		{name: "reference beats default", opts: withDefault, spec: "cp2", want: "cp2"},
		{name: "default", opts: withDefault, want: "default"},
		{name: "legacy flags keep unreferenced organizations", opts: both, want: ""},
		{name: "legacy", opts: legacy, want: ""},
		{name: "missing reference", opts: legacy, spec: "gone", missing: true},
		{name: "missing default", opts: Options{DefaultControlPlane: "gone"}, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := organizationReconciler(t, tc.opts, servingControlPlane("default"), servingControlPlane("cp2"))
			o := organization()
			o.Spec.ControlPlaneRef, o.Status.ControlPlaneRef = tc.spec, tc.status
			got, err := r.controlPlaneFor(ctx, o)
			var missing *missingControlPlaneError
			if tc.missing {
				if !errors.As(err, &missing) {
					t.Fatalf("err = %v, want missing ControlPlane", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if got != (controlPlaneTarget{namespace: "tenant-aep-cp", cluster: "aepcp"}) {
					t.Fatalf("legacy target %+v", got)
				}
				return
			}
			if !got.managed() || got.cp.Name != tc.want || got.namespace != "tenant-"+tc.want || got.cluster != "aepcp" {
				t.Fatalf("target %+v", got)
			}
		})
	}
}

func TestControlPlaneNotReady(t *testing.T) {
	cp := servingControlPlane("default")
	if reason := controlPlaneNotReady(cp); reason != "" {
		t.Fatal(reason)
	}
	for _, mutate := range []func(*api.ControlPlane){
		func(cp *api.ControlPlane) {
			setControlPlaneCondition(cp, "GatewayReady", false, "Pending", "restarting cluster-gateway")
		},
		func(cp *api.ControlPlane) { meta.RemoveStatusCondition(&cp.Status.Conditions, "Hardened") },
		func(cp *api.ControlPlane) { cp.Generation++ }, // conditions of an older spec
		func(cp *api.ControlPlane) { cp.Status.Outputs.KubeconfigSecretRef = nil },
		func(cp *api.ControlPlane) { cp.Status.Gateway.URL = "" },
	} {
		c := servingControlPlane("default")
		mutate(c)
		if controlPlaneNotReady(c) == "" {
			t.Fatalf("not ready control plane accepted: %+v", c.Status)
		}
	}
	c := servingControlPlane("default")
	setControlPlaneCondition(c, "OpenChoreoReady", false, "Pending", "waiting for HelmRelease")
	if reason := controlPlaneNotReady(c); !strings.Contains(reason, "OpenChoreoReady: waiting for HelmRelease") {
		t.Fatal(reason)
	}
}

// An Organization on a ControlPlane that is not ready yet keeps its tenants
// and waits before its clusters with reason ControlPlaneNotReady.
func TestOrganizationWaitsForControlPlane(t *testing.T) {
	ctx := context.Background()
	cp := servingControlPlane("default")
	setControlPlaneCondition(cp, "GatewayReady", false, "Pending", "restarting cluster-gateway")
	o := organization()
	o.Spec.Profile = "small"
	o.Spec.Environments = o.Spec.Environments[:1]
	delete(o.Spec.Tiers, "prod")
	o.Spec.ControlPlaneRef = "default"
	r := organizationReconciler(t, Options{Namespace: "aep-system"}, cp, o)
	mgmt, mgmtDyn, _ := fakeCluster()
	r.Management = mgmt
	for _, tenant := range [][2]string{{"tenant-root", "acme"}, {"tenant-acme", "nonprod"}} {
		u := obj(appsAPI, "Tenant", tenant[0], tenant[1], M{})
		u.SetLabels(map[string]string{OwnerLabel: "org-uid"})
		u.Object["status"] = M{"conditions": []any{M{"type": "Ready", "status": "True"}}}
		if _, err := mgmtDyn.Resource(gvr(appsAPI, "tenants")).Namespace(tenant[0]).Create(ctx, u, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme"}}); err != nil {
			t.Fatal(err)
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	clusters := meta.FindStatusCondition(o.Status.Conditions, "ClustersReady")
	if o.Status.Phase != "Pending" || clusters == nil || clusters.Reason != "ControlPlaneNotReady" || !strings.Contains(clusters.Message, "GatewayReady: restarting cluster-gateway") {
		t.Fatalf("phase %s, ClustersReady %+v", o.Status.Phase, clusters)
	}
	if c := meta.FindStatusCondition(o.Status.Conditions, "TenantsReady"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("tenants must not wait for the control plane: %+v", c)
	}
	if c := meta.FindStatusCondition(o.Status.Conditions, "NetworkPathsReady"); c == nil || c.Reason != "DependencyPending" {
		t.Fatalf("later steps must wait: %+v", c)
	}
	// The ControlPlane's hardening result is reported, not inspected again.
	if c := meta.FindStatusCondition(o.Status.Conditions, "ControlPlaneHardened"); c == nil || c.Status != metav1.ConditionTrue || c.Message != "ControlPlane default: Desired state verified" {
		t.Fatalf("ControlPlaneHardened %+v", c)
	}
	if o.Status.ControlPlaneRef != "default" {
		t.Fatal("accepted reference not recorded")
	}
	if _, err := mgmt.get(ctx, appsAPI, "kuberneteses", "tenant-acme-nonprod", "apps"); err == nil {
		t.Fatal("tier cluster created before the control plane was ready")
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(cp), cp); err != nil {
		t.Fatal(err)
	}
	setControlPlaneCondition(cp, "Hardened", false, "BootstrapBindingsPresent", "Offending ClusterAuthzRoleBindings: groups")
	if err := r.Status().Update(ctx, cp); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if c := meta.FindStatusCondition(o.Status.Conditions, "ControlPlaneHardened"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "BootstrapBindingsPresent" {
		t.Fatalf("ControlPlaneHardened %+v", c)
	}
}

func TestOrganizationRejectsMissingControlPlaneAndCollisions(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, ref, want string
		planes          []client.Object
	}{
		{name: "missing reference", ref: "gone", want: "controlPlaneRef: ControlPlane gone does not exist"},
		{name: "tenant namespace of the organization", planes: []client.Object{servingControlPlane("acme")}, want: "tenant namespace tenant-acme is used by both ControlPlane acme and Organization acme"},
		{name: "tier namespace", planes: []client.Object{func() client.Object {
			cp := servingControlPlane("tiers")
			cp.Spec.Tenant.Parent, cp.Spec.Tenant.Name = "tenant-acme", "prod"
			return cp
		}()}, want: "tenant namespace tenant-acme-prod is used by both ControlPlane tiers and Organization acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := organization()
			o.Spec.ControlPlaneRef = tc.ref
			r := organizationReconciler(t, Options{CPSecret: "aep-system/aepcp"}, append(tc.planes, o)...)
			r.Management, _, _ = fakeCluster()
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme"}}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
				t.Fatal(err)
			}
			c := meta.FindStatusCondition(o.Status.Conditions, "TenantsReady")
			if o.Status.Phase != "Rejected" || c == nil || c.Reason != "InvalidSpec" || c.Message != tc.want {
				t.Fatalf("phase %s condition %+v", o.Status.Phase, c)
			}
			if len(o.Finalizers) != 0 {
				t.Fatal("rejected Organization was taken over")
			}
		})
	}
}

func TestControlPlaneRejectsOrganizationNamespace(t *testing.T) {
	o := organization()
	o.Name = "aepcp" // tenant-aepcp is also the ControlPlane tenant namespace
	f := newCPFixture(t, o)
	if _, err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(f.cp.Status.Conditions, "Ready")
	if f.cp.Status.Phase != "Rejected" || ready == nil || ready.Reason != "InvalidSpec" || !strings.Contains(ready.Message, "tenant namespace tenant-aepcp is used by both ControlPlane default and Organization aepcp") {
		t.Fatalf("phase %s Ready %+v", f.cp.Status.Phase, ready)
	}
	if _, err := f.mgmt.get(f.ctx, appsAPI, "tenants", "tenant-root", "aepcp"); err == nil {
		t.Fatal("tenant created for a colliding control plane")
	}
}

func TestManagedControlPlaneCredential(t *testing.T) {
	ctx := context.Background()
	cp := servingControlPlane("default")
	r := organizationReconciler(t, Options{Namespace: "aep-system"}, cp)
	mgmt, _, kube := fakeCluster()
	r.Management = mgmt
	s := &session{r: r, o: organization()}
	s.o.Spec.ControlPlaneRef = "default"
	if err := s.controlPlane(ctx); err == nil {
		t.Fatal("connected without a credential")
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "operator-access-cp-default", Namespace: "aep-system", Labels: map[string]string{ControlPlaneLabel: "someone-else"}}, Data: map[string][]byte{"kubeconfig": adminKubeconfig(t)}}
	if _, err := kube.CoreV1().Secrets("aep-system").Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.controlPlane(ctx); err == nil || !strings.Contains(err.Error(), "not owned by ControlPlane default") {
		t.Fatalf("foreign credential accepted: %v", err)
	}
	sec.Labels[ControlPlaneLabel] = "default-uid"
	if _, err := kube.CoreV1().Secrets("aep-system").Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.controlPlane(ctx); err != nil || s.cp == nil || s.cp.Config.Host != "https://kubernetes-aepcp.tenant-aepcp.svc:6443" {
		t.Fatal(err)
	}
}

// A managed Organization reaches the gateway mirror of its ControlPlane and
// dials the host the ControlPlane placed in the gateway certificate.
func TestManagedOrganizationNetwork(t *testing.T) {
	f := newCPFixture(t)
	f.seedGateway()
	cp := servingControlPlane("default")
	cp.Spec.Tenant.Name = "aepcp"
	cp.Status.Outputs.TenantNamespace = cpNS
	cp.Status.Gateway.Host, cp.Status.Gateway.URL = gatewayTestHost, "wss://"+gatewayTestHost+":8443/ws"
	s := &session{r: &Reconciler{Management: f.mgmt, Options: Options{GatewayHostTemplate: "unused.{{.DashedIP}}.example"}}, o: organization(), cp: f.remote,
		resolved: &controlPlaneTarget{cp: cp, namespace: cpNS, cluster: "aepcp"}}
	if err := s.network(f.ctx); err != nil {
		t.Fatal(err)
	}
	if s.gateway != cp.Status.Gateway.URL {
		t.Fatal(s.gateway)
	}
	s.resolved.cp.Status.Gateway.Host = "cluster-gateway.other.aep.internal"
	if err := s.network(f.ctx); err == nil || !strings.Contains(err.Error(), "waiting for ControlPlane default to serve a cluster-gateway certificate for cluster-gateway.other.aep.internal") {
		t.Fatal(err)
	}
}

func TestOrganizationsForControlPlane(t *testing.T) {
	served, referencing, colliding, legacy := organization(), organization(), organization(), organization()
	served.Name = "served"
	referencing.Name, referencing.Spec.ControlPlaneRef = "referencing", "cp2"
	colliding.Name = "cp2" // tenant-cp2 is the tenant namespace of ControlPlane cp2
	legacy.Name, legacy.Spec.ControlPlaneRef = "legacy", "other"
	r := organizationReconciler(t, Options{DefaultControlPlane: "default"}, served, referencing, colliding, legacy)
	names := func(cp *api.ControlPlane) []string {
		var out []string
		for _, req := range r.organizationsForControlPlane(context.Background(), cp) {
			out = append(out, req.Name)
		}
		return out
	}
	if got := names(servingControlPlane("cp2")); !reflect.DeepEqual(got, []string{"cp2", "referencing"}) {
		t.Fatal(got)
	}
	if got := names(servingControlPlane("default")); !reflect.DeepEqual(got, []string{"cp2", "served"}) {
		// The colliding Organization also defaults to ControlPlane default.
		t.Fatal(got)
	}
	if got := r.organizationsForControlPlane(context.Background(), &corev1.Service{}); got != nil {
		t.Fatal(got)
	}
	p := controlPlaneChangePredicate()
	old := servingControlPlane("default")
	same := old.DeepCopy()
	same.ResourceVersion = "2"
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: same}) {
		t.Fatal("metadata churn must not enqueue")
	}
	changed := old.DeepCopy()
	setControlPlaneCondition(changed, "Hardened", false, "BootstrapBindingsPresent", "groups")
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: changed}) || !p.Create(event.CreateEvent{Object: old}) || !p.Delete(event.DeleteEvent{Object: old}) {
		t.Fatal("status changes, creation and deletion enqueue")
	}
}
