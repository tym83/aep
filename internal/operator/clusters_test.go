package operator

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeClusterOwner records what waitCluster asks of its owner.
type fakeClusterOwner struct {
	cluster     *Cluster
	since       time.Time
	cleared     bool
	remediated  []string
	connections int
}

func (o *fakeClusterOwner) label() string { return "demo" }
func (o *fakeClusterOwner) readySince() time.Time {
	if o.since.IsZero() {
		o.since = time.Now()
	}
	return o.since
}
func (o *fakeClusterOwner) notReady() { o.since, o.cleared = time.Time{}, true }
func (o *fakeClusterOwner) connect(context.Context) (*Cluster, error) {
	o.connections++
	return o.cluster, nil
}
func (o *fakeClusterOwner) remediate(_ context.Context, ns, release, reason string) error {
	o.remediated = append(o.remediated, ns+"/"+release+":"+reason)
	return nil
}

func TestWaitCluster(t *testing.T) {
	ctx := context.Background()
	mgmt, mgmtDyn, _ := fakeCluster()
	remote, _, remoteKube := fakeCluster()
	owner := &fakeClusterOwner{cluster: remote}
	wait := func(want string) {
		t.Helper()
		err := waitCluster(ctx, mgmt, owner, "tenant-demo", "demo", 2)
		if want == "" {
			if err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, ok := err.(*pendingError); !ok || !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v, want pending %q", err, want)
		}
	}
	wait("waiting for demo TenantControlPlane to be created")
	tcp := obj("kamaji.clastix.io/v1alpha1", "TenantControlPlane", "tenant-demo", "kubernetes-demo", nil)
	if _, err := mgmtDyn.Resource(gvr(tcp.GetAPIVersion(), "tenantcontrolplanes")).Namespace("tenant-demo").Create(ctx, tcp, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	owner.since = time.Now()
	wait("waiting for demo control plane version Ready")
	if !owner.cleared || !owner.since.IsZero() || owner.connections != 0 {
		t.Fatal("a control plane that is not Ready resets the timer and is not contacted", owner)
	}
	tcp.Object["status"] = M{"kubernetesResources": M{"version": M{"status": "Ready"}}}
	if _, err := mgmtDyn.Resource(gvr(tcp.GetAPIVersion(), "tenantcontrolplanes")).Namespace("tenant-demo").Update(ctx, tcp, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	wait("waiting for cluster addon HelmReleases in tenant-demo")
	releases := mgmtDyn.Resource(gvr(helmAPI, "helmreleases")).Namespace("tenant-demo")
	// Releases delivered by AEP target the cluster but are no addons.
	for _, label := range []string{OwnerLabel, ControlPlaneLabel} {
		hr := obj(helmAPI, "HelmRelease", "tenant-demo", "aep-"+strings.ReplaceAll(label, "/", "-"), M{})
		hr.SetLabels(map[string]string{"cozystack.io/target-cluster-name": "kubernetes-demo", label: "uid"})
		if _, err := releases.Create(ctx, hr, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	wait("waiting for cluster addon HelmReleases in tenant-demo")
	addon := obj(helmAPI, "HelmRelease", "tenant-demo", "kubernetes-demo-cilium", M{})
	addon.Object["status"] = M{"conditions": []any{M{"type": "Ready", "status": "False", "message": "context deadline exceeded"}}}
	if _, err := releases.Create(ctx, addon, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	wait("waiting for HelmRelease tenant-demo/kubernetes-demo-cilium")
	if len(owner.remediated) != 1 || owner.remediated[0] != "tenant-demo/kubernetes-demo-cilium:AddonAPITimeout" {
		t.Fatal(owner.remediated)
	}
	addon.Object["status"] = M{"conditions": []any{M{"type": "Ready", "status": "True"}}}
	if _, err := releases.Update(ctx, addon, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	wait("waiting for 2 Ready nodes in tenant-demo")
	// A cluster without nodes or bootstrap token for ten minutes is force-reconciled.
	owner.since = time.Now().Add(-11 * time.Minute)
	wait("waiting for 2 Ready nodes")
	if len(owner.remediated) != 2 || owner.remediated[1] != "tenant-demo/kubernetes-demo:MissingBootstrapToken" {
		t.Fatal(owner.remediated)
	}
	for _, name := range []string{"a", "b"} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		if _, err := remoteKube.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	wait("")
	if phase, _, _ := unstructured.NestedString(tcp.Object, "status", "kubernetesResources", "version", "status"); phase != "Ready" {
		t.Fatal(phase)
	}
}

// Organization tiers keep their timers and remediation history in status.
func TestTierClusterOwner(t *testing.T) {
	s := &session{o: organization(), r: &Reconciler{}}
	owner := tierCluster{s: s, tier: "prod"}
	first := owner.readySince()
	if got := s.o.Status.ControlPlaneReadySince["prod"].Time; !got.Equal(first) || !owner.readySince().Equal(first) {
		t.Fatal("ready time must be recorded once", got, first)
	}
	owner.notReady()
	if _, ok := s.o.Status.ControlPlaneReadySince["prod"]; ok {
		t.Fatal("not Ready must forget the time")
	}
}
