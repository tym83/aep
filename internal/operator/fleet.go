package operator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/change"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"slices"
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
	return fmt.Sprintf("ext-%x", sha256.Sum256([]byte(ec.Namespace+"/"+ec.Name+"/"+string(ec.UID))))[:36]
}
func (f *FleetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ec := &api.ExternalCluster{}
	if e := f.Get(ctx, req.NamespacedName, ec); e != nil {
		return ctrl.Result{}, client.IgnoreNotFound(e)
	}
	if !ec.DeletionTimestamp.IsZero() {
		return f.finalizeFleet(ctx, ec)
	}
	if !slices.Contains(ec.Finalizers, fleetFinalizer) {
		base := ec.DeepCopy()
		ec.Finalizers = append(ec.Finalizers, fleetFinalizer)
		if err := f.Patch(ctx, ec, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}
	before := ec
	ec = ec.DeepCopy()
	for _, t := range fleetConditions {
		if meta.FindStatusCondition(ec.Status.Conditions, t) == nil {
			fleetCondition(ec, t, false, "Pending", "Waiting for observed readiness")
		}
	}
	e := f.reconcileFleet(ctx, ec)
	if e != nil {
		fleetCondition(ec, "Ready", false, "ReconcileFailed", e.Error())
	} else {
		ok := true
		for _, t := range fleetConditions {
			ok = ok && meta.IsStatusConditionTrue(ec.Status.Conditions, t)
		}
		fleetCondition(ec, "Ready", ok, "Observed", "Aggregated fleet readiness")
	}
	if !reflect.DeepEqual(before.Status, ec.Status) {
		if err := f.Status().Update(ctx, ec); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: time.Minute}, e
}

var fleetConditions = []string{"Registered", "Joined", "DataPlaneConnected", "PoliciesReady"}

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
	if ec.Spec.Suspended {
		return f.quarantine(ctx, ec, "Assignment suspended")
	}
	o := &api.Organization{}
	if e := f.Get(ctx, client.ObjectKey{Name: ec.Spec.Organization}, o); e != nil {
		return e
	}
	// Prevent concurrent, approved assignments from racing over one Environment.
	var list api.ExternalClusterList
	if e := f.List(ctx, &list, client.InNamespace(ec.Namespace)); e != nil {
		return e
	}

	for _, other := range list.Items {
		if other.Name == ec.Name || other.Spec.Organization != o.Name || other.Spec.Environment != ec.Spec.Environment {
			continue
		}
		if !other.DeletionTimestamp.IsZero() {
			fleetCondition(ec, "Registered", false, "Pending", "Waiting for assignment detach: "+other.Name)
			return nil
		}
		err := change.VerifyExternalApproval(ctx, f.Client, &other)
		if err != nil {
			var mismatch *change.ApprovalMismatchError
			if errors.As(err, &mismatch) {
				continue
			}
			return err
		}
		if assignmentBefore(&other, ec) {
			fleetCondition(ec, "Registered", false, "Pending", "Waiting for older approved assignment: "+other.Name)
			return nil
		}
	}
	if e := change.VerifyExternalApproval(ctx, f.Client, ec); e != nil {
		var mismatch *change.ApprovalMismatchError
		if !errors.As(e, &mismatch) {
			return e
		}
		if q := f.quarantine(ctx, ec, e.Error()); q != nil {
			return fmt.Errorf("%v; quarantine: %w", e, q)
		}
		return e
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
	if e := f.applyManagedCluster(ctx, o, mc); e != nil {
		return e
	}
	profileVersion, e := f.profileVersion()
	if e != nil {
		return e
	}
	profile := obj(profileVersion, "ClusterProfile", set, ec.Spec.ClusterProfileRef.Name, M{"displayName": ec.Name, "clusterManager": M{"name": "aep-ocm"}})
	profile.SetLabels(map[string]string{"x-k8s.io/cluster-manager": "aep-ocm", "aep.aenix.io/trust": ec.Spec.TrustLevel})
	if e := f.Hub.apply(ctx, o, "clusterprofiles", profile); e != nil {
		return e
	}
	if e := f.approveFleetCSR(ctx, ec, id); e != nil {
		return e
	}
	fleetCondition(ec, "Registered", true, "Accepted", "Approved assignment accepted by OCM")
	mc, e = f.Hub.get(ctx, mc.GetAPIVersion(), "managedclusters", "", id)
	if e != nil {
		return e
	}
	if ocmCondition(mc, "ManagedClusterJoined") {
		if e := f.cleanupBootstrap(ctx, ec); e != nil {
			return e
		}
	}
	// OCM registration conditions do not carry observedGeneration.
	joined := ocmCondition(mc, "ManagedClusterJoined") && ocmCondition(mc, "ManagedClusterConditionAvailable")
	fleetCondition(ec, "Joined", joined, "Observed", "OCM join and availability observed")
	if !joined {
		fleetCondition(ec, "PoliciesReady", false, "JoinPending", "Waiting for available joined cluster")
		fleetCondition(ec, "DataPlaneConnected", false, "JoinPending", "Waiting for available joined cluster")
		return nil
	}
	if e := f.cleanupBootstrap(ctx, ec); e != nil {
		return e
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
	dp := fleetDataPlane(o, ec, data["tls.crt"])
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
		// External clusters carry only the data plane; Kafka claims need a Cozystack tier.
		if ch.name == StrimziChart {
			continue
		}
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
	bridge, err := f.Renderer.Render(ctx, "aep-bridge-appcluster", "0.1.3", "aep-system", M{})
	if err != nil {
		return err
	}
	manifests = append(manifests, bridge...)
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
	var bundle []any
	for _, name := range []string{"app-cluster-baseline", "app-cluster-" + ec.Spec.Tier} {
		b, err := f.Renderer.Render(ctx, name, PolicyBundleVersion, "kyverno", values[name])
		if err != nil {
			return err
		}
		bundle = append(bundle, b...)
	}

	all := append(append(append([]any{}, manifests...), engine...), bundle...)
	permissions, e := fleetWorkPermissions(all)
	if e != nil {
		return e
	}
	permissionsReady, e := f.work(ctx, o, id, "aep-permissions", []any{permissions.Object})
	if e != nil {
		return e
	}
	if !permissionsReady {
		fleetCondition(ec, "PoliciesReady", false, "PermissionsPending", "Waiting for work-agent aggregate role")
		return nil
	}
	if _, e = f.work(ctx, o, id, "aep-dataplane", manifests); e != nil {
		return e
	}
	engineReady, e := f.work(ctx, o, id, "aep-kyverno", engine)
	if e != nil {
		return e
	}
	if !engineReady {
		fleetCondition(ec, "PoliciesReady", false, "EnginePending", "Waiting for Kyverno deployment feedback")
		return nil
	}
	policyReady, e := f.work(ctx, o, id, "aep-policies", bundle)
	if e != nil {
		return e
	}
	fleetCondition(ec, "PoliciesReady", policyReady, "ManifestWorkObserved", "Policy work and Ready feedback observed at current generation")
	live, e := s.cp.get(ctx, ocAPI, "dataplanes", o.Name, fleetDataPlaneName(ec))
	if e != nil {
		return e
	}
	connected, _, _ := unstructured.NestedBool(live.Object, "status", "agentConnection", "connected")
	fleetCondition(ec, "DataPlaneConnected", connected, "AgentObserved", "OpenChoreo agent connection observed")
	if !policyReady || !connected {
		return nil
	}
	return s.cp.apply(ctx, o, "environments", fleetEnvironment(o, ec))
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
		configs := fleetFeedback(batch)
		u := obj("work.open-cluster-management.io/v1", "ManifestWork", id, workName, M{"manifestConfigs": configs, "workload": M{"manifests": batch}, "deleteOption": M{"propagationPolicy": "Orphan"}})
		u.SetLabels(map[string]string{"aep.aenix.io/work-batch": name})
		if e := f.Hub.apply(ctx, o, "manifestworks", u); e != nil {
			return false, e
		}
		live, e := f.Hub.get(ctx, u.GetAPIVersion(), "manifestworks", id, workName)
		if e != nil {
			return false, e
		}
		all = all && observed(live, "Applied") && observed(live, "Available") && fleetFeedbackReady(live, configs)
	}
	stale, e := f.Hub.resource("work.open-cluster-management.io/v1", "manifestworks", id).List(ctx, metav1.ListOptions{LabelSelector: "aep.aenix.io/work-batch=" + name})
	if e != nil {
		return false, e
	}
	for _, w := range stale.Items {
		keep := false
		for i := range batches {
			keep = keep || w.GetName() == fmt.Sprintf("%s-%03d", name, i)
		}
		if keep {
			continue
		}
		if w.GetLabels()[OwnerLabel] != string(o.UID) {
			return false, fmt.Errorf("foreign stale work")
		}
		if _, e = f.Hub.resource(w.GetAPIVersion(), "manifestworks", id).Patch(ctx, w.GetName(), types.MergePatchType, []byte(`{"spec":{"deleteOption":{"propagationPolicy":"Orphan"}}}`), metav1.PatchOptions{}); e != nil {
			return false, e
		}
		if e = f.Hub.resource(w.GetAPIVersion(), "manifestworks", id).Delete(ctx, w.GetName(), metav1.DeleteOptions{}); client.IgnoreNotFound(e) != nil {
			return false, e
		}
	}
	return all, nil
}

func fleetDataPlane(o *api.Organization, ec *api.ExternalCluster, ca []byte) *unstructured.Unstructured {
	id := FleetID(ec)
	return obj(ocAPI, "DataPlane", o.Name, fleetDataPlaneName(ec), M{"planeID": id, "clusterAgent": M{"clientCA": M{"value": string(ca)}}, "secretStoreRef": M{"name": "default"}, "gateway": M{"ingress": M{"external": M{"name": "gateway-default", "namespace": "openchoreo-data-plane", "http": M{"host": ec.Spec.GatewayHost, "port": int64(80)}}}}})
}
func fleetEnvironment(o *api.Organization, ec *api.ExternalCluster) *unstructured.Unstructured {
	env := obj(ocAPI, "Environment", o.Name, ec.Spec.Environment, M{"dataPlaneRef": M{"kind": "DataPlane", "name": fleetDataPlaneName(ec)}, "isProduction": ec.Spec.Tier == "prod"})
	for _, decl := range o.Spec.Environments {
		if decl.Name == ec.Spec.Environment {
			env.SetAnnotations(map[string]string{"aep.aenix.io/change-model": decl.ChangeModel})
		}
	}
	return env
}

// The Environment reference is immutable across assignment recreation.
func fleetDataPlaneName(ec *api.ExternalCluster) string {
	return fmt.Sprintf("ext-%x", sha256.Sum256([]byte(ec.Spec.Organization+"/"+ec.Spec.Environment)))[:36]
}

func assignmentBefore(a, b *api.ExternalCluster) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}
