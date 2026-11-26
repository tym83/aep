package operator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/change"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

// FleetReconciler is part of the organization operator, with a separate queue
// so an unavailable spoke does not block tenant provisioning or CA renewal.
type FleetReconciler struct {
	*Reconciler
	Hub      *Cluster
	Renderer FleetRenderer
	Gateway  string
}

func FleetID(ec *api.ExternalCluster) string {
	return fmt.Sprintf("ext-%x", sha256.Sum256([]byte(ec.Namespace+"/"+ec.Name)))[:36]
}
func (f *FleetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ec := &api.ExternalCluster{}
	if e := f.Get(ctx, req.NamespacedName, ec); e != nil {
		return ctrl.Result{}, client.IgnoreNotFound(e)
	}
	// An assignment is intentionally not an automatic destructive detach API.
	if !ec.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	for _, t := range []string{"Registered", "Joined", "DataPlaneConnected", "PoliciesReady"} {
		fleetCondition(ec, t, false, "Pending", "Waiting for approved assignment and observed readiness")
	}
	e := f.reconcileFleet(ctx, ec)
	if e != nil {
		fleetCondition(ec, "Registered", false, "ReconcileFailed", e.Error())
	}
	if err := f.Status().Update(ctx, ec); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Minute}, e
}
func fleetCondition(ec *api.ExternalCluster, t string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&ec.Status.Conditions, metav1.Condition{Type: t, Status: status, ObservedGeneration: ec.Generation, Reason: reason, Message: msg})
}
func observed(u *unstructured.Unstructured, t string) bool {
	cs, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, v := range cs {
		c, _ := v.(map[string]any)
		if c["type"] == t && c["status"] == "True" {
			g, _ := c["observedGeneration"].(int64)
			return g >= u.GetGeneration()
		}
	}
	return false
}
func (f *FleetReconciler) reconcileFleet(ctx context.Context, ec *api.ExternalCluster) error {
	if e := change.VerifyExternalApproval(ctx, f.Client, ec); e != nil {
		return e
	}
	o := &api.Organization{}
	if e := f.Get(ctx, client.ObjectKey{Name: ec.Spec.Organization}, o); e != nil {
		return e
	}
	// Prevent concurrent, approved assignments from racing over one Environment.
	var list api.ExternalClusterList
	if e := f.List(ctx, &list); e != nil {
		return e
	}
	for _, other := range list.Items {
		if other.UID != ec.UID && other.Spec.Organization == o.Name && other.Spec.Environment == ec.Spec.Environment {
			return fmt.Errorf("environment already assigned to %s", other.Name)
		}
	}
	id := FleetID(ec)
	ec.Status.PlaneID = id
	set := "aep-" + o.Name
	s := &session{r: f.Reconciler, o: o}
	for _, item := range []struct {
		r string
		u *unstructured.Unstructured
	}{
		{"managedclustersets", obj("cluster.open-cluster-management.io/v1beta2", "ManagedClusterSet", "", set, M{"clusterSelector": M{"selectorType": "ExclusiveClusterSetLabel"}})},
		{"namespaces", obj("v1", "Namespace", "", set, nil)},
		{"managedclustersetbindings", obj("cluster.open-cluster-management.io/v1beta2", "ManagedClusterSetBinding", set, set, M{"clusterSet": set})},
		{"placements", obj("cluster.open-cluster-management.io/v1beta1", "Placement", set, id, M{"clusterSets": []any{set}, "numberOfClusters": int64(1), "predicates": []any{M{"requiredClusterSelector": M{"labelSelector": M{"matchLabels": M{"aep.aenix.io/fleet-id": id}}}}}})},
	} {
		if item.u.GetKind() == "Namespace" {
			item.u.SetLabels(map[string]string{"multicluster.x-k8s.io/clusterset": set})
		}
		if e := f.Hub.apply(ctx, o, item.r, item.u); e != nil {
			return e
		}
	}
	mc := obj("cluster.open-cluster-management.io/v1", "ManagedCluster", "", id, M{"hubAcceptsClient": true, "leaseDurationSeconds": int64(60)})
	mc.SetLabels(map[string]string{"cluster.open-cluster-management.io/clusterset": set, "aep.aenix.io/fleet-id": id, "aep.aenix.io/trust": ec.Spec.TrustLevel})
	if e := f.Hub.apply(ctx, o, "managedclusters", mc); e != nil {
		return e
	}
	profile := obj(ec.Spec.ClusterProfileRef.APIVersion, "ClusterProfile", set, ec.Spec.ClusterProfileRef.Name, M{"displayName": ec.Name, "clusterManager": M{"name": "aep-ocm"}})
	profile.SetLabels(map[string]string{"x-k8s.io/cluster-manager": "aep-ocm", "aep.aenix.io/trust": ec.Spec.TrustLevel})
	if e := f.Hub.apply(ctx, o, "clusterprofiles", profile); e != nil {
		return e
	}
	if e := f.approveFleetCSR(ctx, ec, id); e != nil {
		return e
	}
	fleetCondition(ec, "Registered", true, "Accepted", "Approved assignment accepted by OCM")
	mc, e := f.Hub.get(ctx, mc.GetAPIVersion(), "managedclusters", "", id)
	if e != nil {
		return e
	}
	// OCM registration conditions do not carry observedGeneration.
	joined := ocmCondition(mc, "ManagedClusterJoined") && ocmCondition(mc, "ManagedClusterConditionAvailable")
	fleetCondition(ec, "Joined", joined, "Observed", "OCM join and availability observed")
	if !joined {
		return nil
	}
	if e := s.controlPlane(ctx); e != nil {
		return e
	}
	data, e := s.ca(ctx, id)
	if e != nil {
		return e
	}
	leaf, e := f.agentLeaf(ctx, o, id, data)
	if e != nil {
		return e
	}
	gateway, e := s.cp.Kube.CoreV1().Secrets("openchoreo-control-plane").Get(ctx, "cluster-gateway-ca", metav1.GetOptions{})
	if e != nil {
		return e
	}
	if len(gateway.Data["ca.crt"]) == 0 {
		return fmt.Errorf("gateway CA missing")
	}
	dp := obj(ocAPI, "DataPlane", o.Name, id, M{"planeID": id, "clusterAgent": M{"clientCA": M{"value": string(data["tls.crt"])}}, "secretStoreRef": M{"name": "default"}})
	if e = s.cp.apply(ctx, o, "dataplanes", dp); e != nil {
		return e
	}
	// Never put the signing key in ManifestWork. Only the 90-day leaf leaves management.
	ns := obj("v1", "Namespace", "", "openchoreo-data-plane", nil)
	ns.SetLabels(map[string]string{"pod-security.kubernetes.io/enforce": "restricted"})
	cm := obj("v1", "ConfigMap", "openchoreo-data-plane", "cluster-gateway-ca", nil)
	cm.Object["data"] = M{"ca.crt": string(gateway.Data["ca.crt"])}
	manifests := []any{ns.Object, secretObject("openchoreo-data-plane", "cluster-agent-tls", leaf).Object, cm.Object}
	for _, ch := range charts {
		values := M{}
		if ch.name == "openchoreo-data-plane" {
			values = M{"gateway": M{"tls": M{"enabled": false}}, "clusterAgent": M{"planeID": id, "serverUrl": f.Gateway, "tls": M{"generateCerts": false, "clientSecretName": "cluster-agent-tls"}}}
		}
		rendered, err := f.Renderer.Render(ctx, ch.name, ch.version, "openchoreo-data-plane", values)
		if err != nil {
			return err
		}
		for _, manifest := range rendered {
			m, _ := manifest.(map[string]any)
			if m["kind"] == "Deployment" {
				name, _, _ := unstructured.NestedString(m, "metadata", "name")
				if name == "cluster-agent-dataplane" {
					_ = unstructured.SetNestedField(m, fmt.Sprintf("%x", sha256.Sum256(leaf["tls.crt"])), "spec", "template", "metadata", "annotations", "aep.aenix.io/agent-certificate")
				}
			}
		}
		manifests = append(manifests, rendered...)
	}
	bridge, err := f.Renderer.Render(ctx, "aep-bridge-appcluster", "0.1.0", "aep-system", M{})
	if err != nil {
		return err
	}
	manifests = append(manifests, bridge...)
	if _, e = f.work(ctx, o, id, "aep-dataplane", manifests); e != nil {
		return e
	}
	values := policyValues(o, ec.Spec.Tier, o.Spec.Policy.CosignPublicKey)
	if ec.Spec.Tier == "prod" && o.Spec.Policy.CosignPublicKey == "" {
		return errCosignKeyMissing
	}
	if ec.Spec.TrustLevel != "standard" {
		values["app-cluster-baseline"]["mode"] = "enforce"
	}
	kns := obj("v1", "Namespace", "", "kyverno", nil)
	kns.SetLabels(map[string]string{"pod-security.kubernetes.io/enforce": "restricted"})
	engine, e := f.Renderer.Render(ctx, "kyverno", KyvernoChartVersion, "kyverno", values["kyverno"])
	if e != nil {
		return e
	}
	engine = append([]any{kns.Object}, engine...)
	engineReady, e := f.work(ctx, o, id, "aep-kyverno", engine)
	if e != nil {
		return e
	}
	if !engineReady {
		return nil
	}
	var bundle []any
	for _, name := range []string{"app-cluster-baseline", "app-cluster-" + ec.Spec.Tier} {
		b, err := f.Renderer.Render(ctx, name, PolicyBundleVersion, "kyverno", values[name])
		if err != nil {
			return err
		}
		bundle = append(bundle, b...)
	}
	policyReady, e := f.work(ctx, o, id, "aep-policies", bundle)
	if e != nil {
		return e
	}
	fleetCondition(ec, "PoliciesReady", policyReady, "ManifestWorkObserved", "Policy work Applied and Available at current generation")
	live, e := s.cp.get(ctx, ocAPI, "dataplanes", o.Name, id)
	if e != nil {
		return e
	}
	connected, _, _ := unstructured.NestedBool(live.Object, "status", "agentConnection", "connected")
	fleetCondition(ec, "DataPlaneConnected", connected, "AgentObserved", "OpenChoreo agent connection observed")
	if !policyReady || !connected {
		return nil
	}
	env := obj(ocAPI, "Environment", o.Name, ec.Spec.Environment, M{"dataPlaneRef": M{"kind": "DataPlane", "name": id}})
	return s.cp.apply(ctx, o, "environments", env)
}
func ocmCondition(u *unstructured.Unstructured, t string) bool {
	cs, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, v := range cs {
		c, _ := v.(map[string]any)
		if c["type"] == t && c["status"] == "True" {
			return true
		}
	}
	return false
}

// Split works below the API/etcd object size limit. Kyverno's CRDs alone
// exceed a single object limit when placed into one ManifestWork.
func workBatches(manifests []any) ([][]any, error) {
	var batches [][]any
	var batch []any
	size := 0
	for _, m := range manifests {
		b, e := json.Marshal(m)
		if e != nil {
			return nil, e
		}
		if len(b) > 900000 {
			return nil, fmt.Errorf("single fleet manifest exceeds 900KB")
		}
		if size+len(b) > 900000 && len(batch) > 0 {
			batches = append(batches, batch)
			batch = nil
			size = 0
		}
		batch = append(batch, m)
		size += len(b)
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches, nil
}
func (f *FleetReconciler) work(ctx context.Context, o *api.Organization, id, name string, manifests []any) (bool, error) {
	batches, e := workBatches(manifests)
	if e != nil {
		return false, e
	}
	all := len(batches) > 0
	for i, batch := range batches {
		workName := fmt.Sprintf("%s-%03d", name, i)
		u := obj("work.open-cluster-management.io/v1", "ManifestWork", id, workName, M{"workload": M{"manifests": batch}, "deleteOption": M{"propagationPolicy": "Orphan"}})
		if e := f.Hub.apply(ctx, o, "manifestworks", u); e != nil {
			return false, e
		}
		live, e := f.Hub.get(ctx, u.GetAPIVersion(), "manifestworks", id, workName)
		if e != nil {
			return false, e
		}
		all = all && observed(live, "Applied") && observed(live, "Available")
	}
	return all, nil
}
