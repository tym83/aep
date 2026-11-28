package operator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const cpNS = "tenant-aepcp"

// fakeCluster is a dynamic and typed fake cluster. The dynamic fake does not
// implement server-side apply: an apply replaces the object and keeps its
// status, as the API server does for the operator's fields.
func fakeCluster(objects ...runtime.Object) (*Cluster, *dynamicfake.FakeDynamicClient, *kubefake.Clientset) {
	listKinds := map[schema.GroupVersionResource]string{}
	for _, r := range []struct{ av, resource, kind string }{
		{helmAPI, "helmreleases", "HelmReleaseList"},
		{"source.toolkit.fluxcd.io/v1", "ocirepositories", "OCIRepositoryList"},
		{"networking.k8s.io/v1", "ingresses", "IngressList"},
		{"cilium.io/v2", "ciliumclusterwidenetworkpolicies", "CiliumClusterwideNetworkPolicyList"},
		{appsAPI, "kubernetesnodeses", "KubernetesNodesList"},
		{appsAPI, "kuberneteses", "KubernetesList"},
		{appsAPI, "postgreses", "PostgresList"},
		{appsAPI, "tenants", "TenantList"},
		{"v1", "secrets", "SecretList"},
		{ocAPI, "clusterauthzrolebindings", "ClusterAuthzRoleBindingList"},
	} {
		listKinds[gvr(r.av, r.resource)] = r.kind
	}
	d := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
	d.PrependReactor("patch", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		p := a.(ktesting.PatchAction)
		if p.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		u := &unstructured.Unstructured{}
		if err := json.Unmarshal(p.GetPatch(), &u.Object); err != nil {
			return true, nil, err
		}
		old, err := d.Tracker().Get(p.GetResource(), p.GetNamespace(), p.GetName())
		if err != nil {
			return true, nil, err
		}
		if status, ok := old.(*unstructured.Unstructured).Object["status"]; ok {
			u.Object["status"] = status
		}
		return true, u, d.Tracker().Update(p.GetResource(), u, p.GetNamespace())
	})
	k := kubefake.NewClientset()
	return &Cluster{Dynamic: d, Kube: k}, d, k
}

type cpFixture struct {
	t                    *testing.T
	ctx                  context.Context
	client               client.Client
	r                    *ControlPlaneReconciler
	mgmt, remote         *Cluster
	mgmtDyn, remoteDyn   *dynamicfake.FakeDynamicClient
	mgmtKube, remoteKube *kubefake.Clientset
	cp                   *api.ControlPlane
}

func newCPFixture(t *testing.T, objects ...client.Object) *cpFixture {
	t.Helper()
	f := &cpFixture{t: t, ctx: context.Background()}
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	f.cp = controlPlane()
	f.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{f.cp}, objects...)...).WithStatusSubresource(&api.ControlPlane{}, &api.Organization{}).Build()
	f.mgmt, f.mgmtDyn, f.mgmtKube = fakeCluster()
	f.remote, f.remoteDyn, f.remoteKube = fakeCluster()
	f.remoteKube.PrependReactor("create", "serviceaccounts", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" || a.GetNamespace() != "kube-system" {
			return false, nil, nil
		}
		return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{Token: "operator-token", ExpirationTimestamp: metav1.NewTime(time.Now().Add(24 * time.Hour))}}, nil
	})
	f.r = &ControlPlaneReconciler{Client: f.client, Management: f.mgmt, Recorder: record.NewFakeRecorder(100),
		Options: Options{Namespace: "aep-system", GatewayHostTemplate: "cluster-gateway.{{.DashedIP}}.nip.io", TenantEgressWorkaround: true,
			PruneBootstrapBindings: true, BootstrapBindingSystemClients: DefaultBootstrapBindingSystemClients},
		Connect: func(cfg *rest.Config) (*Cluster, error) {
			return &Cluster{Dynamic: f.remoteDyn, Kube: f.remoteKube, Config: cfg}, nil
		}}
	f.secret(f.mgmtKube, "aep-system", "aep-portal-client", corev1.SecretTypeOpaque, map[string][]byte{"client-secret": []byte("portal-secret")})
	return f
}

func (f *cpFixture) secret(k *kubefake.Clientset, ns, name string, typ corev1.SecretType, data map[string][]byte) {
	f.t.Helper()
	if _, err := k.CoreV1().Secrets(ns).Create(f.ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Type: typ, Data: data}, metav1.CreateOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cpFixture) reconcile() (ctrl.Result, error) {
	f.t.Helper()
	res, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: f.cp.Name}})
	if e := f.client.Get(f.ctx, client.ObjectKeyFromObject(f.cp), f.cp); e != nil {
		f.t.Fatal(e)
	}
	return res, err
}

// expect reconciles and requires condition t with status and a message containing want.
func (f *cpFixture) expect(t string, ok bool, want string) {
	f.t.Helper()
	if _, err := f.reconcile(); err != nil && ok {
		f.t.Fatal(err)
	}
	c := meta.FindStatusCondition(f.cp.Status.Conditions, t)
	if c == nil || (c.Status == metav1.ConditionTrue) != ok || !strings.Contains(c.Message, want) {
		f.t.Fatalf("%s = %+v, want ok=%v %q; conditions %+v", t, c, ok, want, f.cp.Status.Conditions)
	}
}

func (f *cpFixture) markReady(d *dynamicfake.FakeDynamicClient, av, resource, ns, name string) {
	f.t.Helper()
	ri := d.Resource(gvr(av, resource)).Namespace(ns)
	u, err := ri.Get(f.ctx, name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	u.Object["status"] = M{"conditions": []any{M{"type": "Ready", "status": "True"}}}
	if _, err = ri.Update(f.ctx, u, metav1.UpdateOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cpFixture) create(d *dynamicfake.FakeDynamicClient, resource string, u *unstructured.Unstructured) {
	f.t.Helper()
	if _, err := d.Resource(gvr(u.GetAPIVersion(), resource)).Namespace(u.GetNamespace()).Create(f.ctx, u, metav1.CreateOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

func adminKubeconfig(t *testing.T) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["aepcp"] = &clientcmdapi.Cluster{Server: "https://kubernetes-aepcp.tenant-aepcp.svc:6443"}
	cfg.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "super-admin"}
	cfg.Contexts["aepcp"] = &clientcmdapi.Context{Cluster: "aepcp", AuthInfo: "admin"}
	cfg.CurrentContext = "aepcp"
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seedCluster makes the aep-cp cluster exist with Ready control plane and addons, and nodes Ready nodes.
func (f *cpFixture) seedCluster(nodes int) {
	tcp := obj("kamaji.clastix.io/v1alpha1", "TenantControlPlane", cpNS, "kubernetes-aepcp", nil)
	tcp.Object["status"] = M{"kubernetesResources": M{"version": M{"status": "Ready"}}}
	f.create(f.mgmtDyn, "tenantcontrolplanes", tcp)
	addon := obj(helmAPI, "HelmRelease", cpNS, "kubernetes-aepcp-cert-manager", M{})
	addon.SetLabels(map[string]string{"cozystack.io/target-cluster-name": "kubernetes-aepcp"})
	addon.Object["status"] = M{"conditions": []any{M{"type": "Ready", "status": "True"}}}
	f.create(f.mgmtDyn, "helmreleases", addon)
	f.secret(f.mgmtKube, cpNS, "kubernetes-aepcp-admin-kubeconfig", corev1.SecretTypeOpaque, map[string][]byte{"super-admin.svc": adminKubeconfig(f.t)})
	f.addNodes(nodes)
}

func (f *cpFixture) addNodes(n int) {
	existing, _ := f.remoteKube.CoreV1().Nodes().List(f.ctx, metav1.ListOptions{})
	for i := range n {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "aepcp-md0-" + string(rune('a'+len(existing.Items)+i))}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		if _, err := f.remoteKube.CoreV1().Nodes().Create(f.ctx, node, metav1.CreateOptions{}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *cpFixture) seedDatabase() {
	f.markReady(f.mgmtDyn, appsAPI, "postgreses", cpNS, "portal")
	f.secret(f.mgmtKube, cpNS, "postgres-portal-credentials", corev1.SecretTypeOpaque, map[string][]byte{"portal": []byte("db-password")})
	if _, err := f.mgmtKube.CoreV1().Services(cpNS).Create(f.ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "postgres-portal-rw", Namespace: cpNS}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.10.20"}}, metav1.CreateOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

const gatewayTestHost = "cluster-gateway.10-96-57-50.nip.io"

func gatewayCert(t *testing.T, host string, serial int64, notBefore time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "cluster-gateway"}, DNSNames: []string{"cluster-gateway.openchoreo-control-plane.svc", host}, NotBefore: notBefore, NotAfter: notBefore.Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// seedGateway adds the management mirror of cluster-gateway and the aep-cp
// gateway Deployment serving a certificate for the template host, with a pod
// started before that certificate was issued.
func (f *cpFixture) seedGateway() {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes-aepcp-cluster-gateway-external", Namespace: cpNS,
		Labels: map[string]string{"cluster.x-k8s.io/tenant-service-name": "cluster-gateway-external", "cluster.x-k8s.io/cluster-name": "kubernetes-aepcp"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, ClusterIP: "10.96.57.50", Ports: []corev1.ServicePort{{Port: 8443, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(30268)}}}}
	if _, err := f.mgmtKube.CoreV1().Services(cpNS).Create(f.ctx, svc, metav1.CreateOptions{}); err != nil {
		f.t.Fatal(err)
	}
	issued := time.Now().Add(-time.Hour)
	f.secret(f.remoteKube, OpenChoreoNamespace, "cluster-gateway-tls", corev1.SecretTypeTLS, map[string][]byte{"tls.crt": gatewayCert(f.t, gatewayTestHost, 0x1f, issued)})
	replicas := int32(1)
	labels := map[string]string{"app": "cluster-gateway", "app.kubernetes.io/component": "cluster-gateway"}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cluster-gateway", Namespace: OpenChoreoNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       corev1.PodSpec{Volumes: []corev1.Volume{{Name: "server-certs", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "cluster-gateway-tls"}}}}}}}}
	if _, err := f.remoteKube.AppsV1().Deployments(OpenChoreoNamespace).Create(f.ctx, dep, metav1.CreateOptions{}); err != nil {
		f.t.Fatal(err)
	}
	started := metav1.NewTime(issued.Add(-time.Hour))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cluster-gateway-1", Namespace: OpenChoreoNamespace, Labels: labels}, Status: corev1.PodStatus{StartTime: &started}}
	if _, err := f.remoteKube.CoreV1().Pods(OpenChoreoNamespace).Create(f.ctx, pod, metav1.CreateOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cpFixture) rollOutGateway() {
	dep, err := f.remoteKube.AppsV1().Deployments(OpenChoreoNamespace).Get(f.ctx, "cluster-gateway", metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	dep.Status = appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	if _, err = f.remoteKube.AppsV1().Deployments(OpenChoreoNamespace).UpdateStatus(f.ctx, dep, metav1.UpdateOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

const gatewayDefaultMirror = "afecde7106db04f419dfbec1a322e8f3"

func (f *cpFixture) seedIngressMirror() {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: gatewayDefaultMirror, Namespace: cpNS, Labels: map[string]string{
		"cluster.x-k8s.io/tenant-service-name": "gateway-default", "cluster.x-k8s.io/tenant-service-namespace": OpenChoreoNamespace, "cluster.x-k8s.io/cluster-name": "kubernetes-aepcp"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, ClusterIP: "10.96.80.80", Ports: []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}}}}
	if _, err := f.mgmtKube.CoreV1().Services(cpNS).Create(f.ctx, svc, metav1.CreateOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cpFixture) release(name string) *unstructured.Unstructured {
	f.t.Helper()
	hr, err := f.mgmt.get(f.ctx, helmAPI, "helmreleases", cpNS, name)
	if err != nil {
		f.t.Fatal(err)
	}
	return hr
}

// converge drives a ControlPlane through every step, checking each pending
// state, until it is Ready.
func (f *cpFixture) converge() {
	f.t.Helper()
	f.install()
	// Content does not hold hardening, which Organizations wait for.
	f.expect("ContentReady", false, "waiting for HelmRelease tenant-aepcp/aep-cp-aep-component-types Ready")
	f.expect("Hardened", true, "")
	if f.cp.Status.Phase != "Pending" {
		f.t.Fatal("content must hold Ready", f.cp.Status.Phase)
	}
	f.markReady(f.mgmtDyn, helmAPI, "helmreleases", cpNS, "aep-cp-aep-component-types")
	f.expect("Ready", true, "Control plane is ready")
}

// install drives a ControlPlane through every step up to its content.
func (f *cpFixture) install() {
	f.t.Helper()
	f.expect("TenantReady", false, "waiting for Tenant tenant-root/aepcp Ready")
	if !slices.Contains(f.cp.Finalizers, ControlPlaneFinalizer) {
		f.t.Fatal("finalizer missing")
	}
	if ready := meta.FindStatusCondition(f.cp.Status.Conditions, "Ready"); ready == nil || ready.Status != metav1.ConditionFalse || !strings.HasPrefix(ready.Message, "TenantReady:") {
		f.t.Fatalf("Ready must name the first pending step: %+v", ready)
	}
	f.markReady(f.mgmtDyn, appsAPI, "tenants", "tenant-root", "aepcp")
	f.expect("ClusterReady", false, "waiting for aepcp TenantControlPlane to be created")
	f.expect("NetworkPathsReady", true, "")
	f.seedCluster(2)
	f.expect("ClusterReady", false, "waiting for 3 Ready nodes in tenant-aepcp")
	f.addNodes(1)
	f.expect("PrerequisitesReady", false, "waiting for HelmRelease tenant-aepcp/aep-cp-kgateway-crds Ready")
	f.expect("AccessReady", true, "")
	f.markReady(f.mgmtDyn, helmAPI, "helmreleases", cpNS, "aep-cp-kgateway-crds")
	f.markReady(f.mgmtDyn, helmAPI, "helmreleases", cpNS, "aep-cp-kgateway")
	f.expect("OpenChoreoReady", false, "waiting for Postgres tenant-aepcp/portal Ready")
	f.seedDatabase()
	f.expect("OpenChoreoReady", false, "waiting for HelmRelease tenant-aepcp/aep-cp-openchoreo-control-plane Ready")
	f.markReady(f.mgmtDyn, helmAPI, "helmreleases", cpNS, "aep-cp-openchoreo-control-plane")
	f.expect("GatewayReady", false, "expected exactly one gateway mirror Service in tenant-aepcp")
	f.seedGateway()
	f.expect("GatewayReady", false, "waiting for the control plane chart to add "+gatewayTestHost)
	names, _, _ := unstructured.NestedSlice(f.release("aep-cp-openchoreo-control-plane").Object, "spec", "values", "clusterGateway", "tls", "dnsNames")
	if !slices.Contains(names, any(gatewayTestHost)) {
		f.t.Fatal("gateway host missing from the chart values", names)
	}
	f.expect("GatewayReady", false, "restarting cluster-gateway to serve certificate 1f")
	f.expect("GatewayReady", false, "waiting for cluster-gateway to restart with certificate 1f")
	f.rollOutGateway()
	f.expect("IngressReady", false, "expected exactly one mirror Service of gateway-default in tenant-aepcp, found 0")
	f.expect("GatewayReady", true, "wss://"+gatewayTestHost+":8443/ws")
	f.seedIngressMirror()
	f.expect("IngressReady", false, "waiting for TLS Secret tenant-aepcp/aep-cp-public-tls")
	f.secret(f.mgmtKube, cpNS, "aep-cp-public-tls", corev1.SecretTypeTLS, map[string][]byte{"tls.crt": []byte("cert")})
}

func TestControlPlaneConverges(t *testing.T) {
	f := newCPFixture(t)
	f.converge()
	st := f.cp.Status
	if st.Phase != "Ready" || st.Outputs.TenantNamespace != cpNS || st.Outputs.ClusterName != "aepcp" || st.Outputs.KubeconfigSecretRef == nil || st.Outputs.KubeconfigSecretRef.Name != "operator-access-cp-default" {
		t.Fatalf("outputs %+v", st)
	}
	if st.Endpoints != (api.ControlPlaneEndpoints{Console: "https://console.cp.example.com", API: "https://api.cp.example.com", APIHost: "api.cp.example.com", InternalAPI: "http://" + gatewayDefaultMirror + "." + cpNS + ".svc"}) {
		t.Fatalf("endpoints %+v", st.Endpoints)
	}
	if st.Gateway != (api.ControlPlaneGatewayStatus{Host: gatewayTestHost, URL: "wss://" + gatewayTestHost + ":8443/ws", MirrorService: cpNS + "/kubernetes-aepcp-cluster-gateway-external", TargetPort: 30268, ServedCertSerial: "1f"}) {
		t.Fatalf("gateway %+v", st.Gateway)
	}
	for _, tc := range []struct {
		t, reason string
		ok        bool
	}{{"ContentReady", "Reconciled", true}, {"AuditReady", "Disabled", true}, {"Hardened", "Reconciled", true}} {
		if c := meta.FindStatusCondition(st.Conditions, tc.t); c == nil || (c.Status == metav1.ConditionTrue) != tc.ok || c.Reason != tc.reason {
			t.Fatalf("%s: %+v", tc.t, c)
		}
	}
	// Management objects carry the ControlPlane label, never the Organization one.
	tenant, err := f.mgmt.get(f.ctx, appsAPI, "tenants", "tenant-root", "aepcp")
	if err != nil || tenant.GetLabels()[ControlPlaneLabel] != "cp-uid" || tenant.GetLabels()[OwnerLabel] != "" {
		t.Fatal(err, tenant.GetLabels())
	}
	hr := f.release("aep-cp-openchoreo-control-plane")
	spec := hr.Object["spec"].(map[string]any)
	if ref := spec["kubeConfig"].(map[string]any)["secretRef"].(map[string]any); ref["name"] != "kubernetes-aepcp-admin-kubeconfig" || ref["key"] != "super-admin.svc" {
		t.Fatal(ref)
	}
	if deps := spec["dependsOn"].([]any); deps[0].(map[string]any)["name"] != "aep-cp-kgateway" || spec["targetNamespace"] != OpenChoreoNamespace || hr.GetLabels()["cozystack.io/target-cluster-name"] != "kubernetes-aepcp" {
		t.Fatal(spec)
	}
	kgateway, _, _ := unstructured.NestedString(f.release("aep-cp-kgateway").Object, "spec", "targetNamespace")
	if kgateway != OpenChoreoNamespace {
		t.Fatal(kgateway)
	}
	ccnp, err := f.mgmt.get(f.ctx, "cilium.io/v2", "ciliumclusterwidenetworkpolicies", "", "aep-cp-default-root-egress")
	if err != nil || !reflect.DeepEqual(ccnp.Object["spec"].(map[string]any)["endpointSelector"], M{"matchExpressions": []any{M{"key": "k8s:io.kubernetes.pod.namespace", "operator": "In", "values": []any{cpNS}}}}) {
		t.Fatal(err, ccnp)
	}
	nodes, err := f.mgmt.get(f.ctx, appsAPI, "kubernetesnodeses", cpNS, "aepcp-md0")
	if err != nil || nodes.Object["spec"].(map[string]any)["minReplicas"] != int64(3) {
		t.Fatal(err, nodes)
	}
	// The portal database is reachable on aep-cp through a selectorless Service.
	slice, err := f.remote.get(f.ctx, "discovery.k8s.io/v1", "endpointslices", OpenChoreoNamespace, PortalDatabaseService)
	if err != nil || slice.GetLabels()["kubernetes.io/service-name"] != PortalDatabaseService || !reflect.DeepEqual(slice.Object["endpoints"], []any{M{"addresses": []any{"10.96.10.20"}, "conditions": M{"ready": true}}}) {
		t.Fatal(err, slice)
	}
	svc, err := f.remote.get(f.ctx, "v1", "services", OpenChoreoNamespace, PortalDatabaseService)
	if _, has := svc.Object["spec"].(map[string]any)["selector"]; err != nil || has {
		t.Fatal("portal database Service must have no selector", err)
	}
	db, err := f.remote.get(f.ctx, "v1", "secrets", OpenChoreoNamespace, PortalDatabaseSecret)
	if err != nil || dataValue(t, db, "password") != "db-password" || dataValue(t, db, "user") != "portal" {
		t.Fatal(err)
	}
	portal, err := f.remote.get(f.ctx, "v1", "secrets", OpenChoreoNamespace, PortalSecret)
	if err != nil || dataValue(t, portal, "client-secret") != "portal-secret" || len(dataValue(t, portal, "backend-secret")) != 64 || dataValue(t, portal, "jenkins-api-key") == "" {
		t.Fatal(err)
	}
	// The scoped identity, not the super-admin, is stored for consumers.
	cred, err := f.mgmt.get(f.ctx, "v1", "secrets", "aep-system", "operator-access-cp-default")
	if err != nil || !strings.Contains(dataValue(t, cred, "kubeconfig"), "operator-token") || strings.Contains(dataValue(t, cred, "kubeconfig"), "super-admin") {
		t.Fatal(err)
	}
	role, err := f.remote.get(f.ctx, "rbac.authorization.k8s.io/v1", "clusterrolebindings", "", "aep-operator")
	if err != nil || !reflect.DeepEqual(role.Object["subjects"], []any{M{"kind": "ServiceAccount", "name": "aep-operator", "namespace": "kube-system"}}) {
		t.Fatal(err, role)
	}
	for _, name := range []string{PublicIngress, DenyLogsIngress} {
		if ing, err := f.mgmt.get(f.ctx, "networking.k8s.io/v1", "ingresses", cpNS, name); err != nil || ing.GetLabels()[ControlPlaneLabel] != "cp-uid" {
			t.Fatal(name, err)
		}
	}
	t.Run("idempotent", func(t *testing.T) {
		releases := map[string]any{}
		for _, name := range []string{"aep-cp-kgateway-crds", "aep-cp-kgateway", "aep-cp-openchoreo-control-plane", "aep-cp-aep-component-types"} {
			releases[name] = f.release(name).Object["spec"]
		}
		backend := dataValue(t, portal, "backend-secret")
		version := f.cp.ResourceVersion
		f.remoteKube.ClearActions()
		f.mgmtDyn.ClearActions()
		f.remoteDyn.ClearActions()
		if _, err := f.reconcile(); err != nil {
			t.Fatal(err)
		}
		for name, spec := range releases {
			if !reflect.DeepEqual(f.release(name).Object["spec"], spec) {
				t.Fatalf("%s changed on a second reconcile", name)
			}
		}
		for _, a := range f.remoteKube.Actions() {
			if a.GetVerb() == "patch" || a.GetVerb() == "update" || a.GetVerb() == "delete" || (a.GetVerb() == "create" && a.GetSubresource() == "") {
				t.Fatalf("second reconcile wrote to aep-cp: %v", a)
			}
		}
		for _, d := range []*dynamicfake.FakeDynamicClient{f.mgmtDyn, f.remoteDyn} {
			for _, a := range d.Actions() {
				if a.GetVerb() == "create" || a.GetVerb() == "delete" || a.GetVerb() == "update" {
					t.Fatalf("second reconcile changed an object: %v", a)
				}
			}
		}
		portal, _ := f.remote.get(f.ctx, "v1", "secrets", OpenChoreoNamespace, PortalSecret)
		if dataValue(t, portal, "backend-secret") != backend {
			t.Fatal("portal session secret was regenerated")
		}
		if f.cp.ResourceVersion != version {
			t.Fatal("unchanged status was rewritten")
		}
	})
}

func TestControlPlaneRefusesUnownedObjects(t *testing.T) {
	f := newCPFixture(t)
	f.create(f.mgmtDyn, "tenants", obj(appsAPI, "Tenant", "tenant-root", "aepcp", M{"etcd": true}))
	if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(f.cp.Status.Conditions, "TenantReady")
	if c == nil || c.Reason != "Retrying" || !strings.Contains(c.Message, "without control plane ownership") {
		t.Fatalf("%+v", c)
	}
	tenant, _ := f.mgmt.get(f.ctx, appsAPI, "tenants", "tenant-root", "aepcp")
	if len(tenant.GetLabels()) != 0 || tenant.Object["spec"].(map[string]any)["etcd"] != true {
		t.Fatal("unowned Tenant was changed", tenant)
	}
}

func TestControlPlaneRefusesOrganizationOwnedRemoteSecret(t *testing.T) {
	f := newCPFixture(t)
	foreign := obj("v1", "Secret", OpenChoreoNamespace, PortalSecret, nil)
	foreign.SetLabels(map[string]string{OwnerLabel: "org-uid"})
	f.create(f.remoteDyn, "secrets", foreign)
	s := f.r.session(f.cp)
	err := s.portalSecrets(f.ctx, f.remote)
	if err == nil || !strings.Contains(err.Error(), "labels of another owner") && !strings.Contains(err.Error(), "refusing") {
		t.Fatal(err)
	}
}

func TestControlPlaneEgressWorkaroundOff(t *testing.T) {
	f := newCPFixture(t)
	f.markTenantAndRun()
	f.r.Options.TenantEgressWorkaround = false
	f.expect("NetworkPathsReady", false, "waiting for ciliumclusterwidenetworkpolicies aep-cp-default-root-egress deletion")
	f.expect("NetworkPathsReady", true, "")
}

func (f *cpFixture) markTenantAndRun() {
	f.t.Helper()
	_, _ = f.reconcile()
	f.markReady(f.mgmtDyn, appsAPI, "tenants", "tenant-root", "aepcp")
	f.expect("NetworkPathsReady", true, "")
}

func TestAuthzBootstrapPullSecret(t *testing.T) {
	f := newCPFixture(t)
	f.r.Options.ImageRegistryMirror, f.r.Options.RegistrySecret = "mirror.local/cache", "mirror-auth"
	s := f.r.session(f.cp)
	if err := s.authzBootstrapPullSecret(f.ctx, f.remote); err != nil {
		t.Fatal("no hook ServiceAccount is a no-op", err)
	}
	helm := map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	if _, err := f.remoteKube.CoreV1().ServiceAccounts(OpenChoreoNamespace).Create(f.ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: authzBootstrapName, Labels: helm}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	failing := corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}
	hook := map[string]string{"app.kubernetes.io/component": "authz-bootstrap"}
	for _, pod := range []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "stuck", Labels: hook}, Status: failing},
		{ObjectMeta: metav1.ObjectMeta{Name: "starting", Labels: hook}},
		{ObjectMeta: metav1.ObjectMeta{Name: "admitted-after-patch", Labels: hook}, Spec: corev1.PodSpec{ImagePullSecrets: []corev1.LocalObjectReference{{Name: MirrorPullSecret}}}, Status: failing},
	} {
		if _, err := f.remoteKube.CoreV1().Pods(OpenChoreoNamespace).Create(f.ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := s.authzBootstrapPullSecret(f.ctx, f.remote); err != nil {
			t.Fatal(err)
		}
	}
	sa, _ := f.remoteKube.CoreV1().ServiceAccounts(OpenChoreoNamespace).Get(f.ctx, authzBootstrapName, metav1.GetOptions{})
	if !reflect.DeepEqual(sa.ImagePullSecrets, []corev1.LocalObjectReference{{Name: MirrorPullSecret}}) {
		t.Fatal(sa.ImagePullSecrets)
	}
	pods, _ := f.remoteKube.CoreV1().Pods(OpenChoreoNamespace).List(f.ctx, metav1.ListOptions{})
	var left []string
	for _, p := range pods.Items {
		left = append(left, p.Name)
	}
	slices.Sort(left)
	if !reflect.DeepEqual(left, []string{"admitted-after-patch", "starting"}) {
		t.Fatal("only pods that cannot pull without the secret are replaced", left)
	}
	f.r.Options.RegistrySecret = ""
	f.remoteKube.ClearActions()
	if err := s.authzBootstrapPullSecret(f.ctx, f.remote); err != nil || len(f.remoteKube.Actions()) != 0 {
		t.Fatal("without an authenticated mirror nothing is touched", err)
	}
}

func TestControlPlaneMirrorPrerequisites(t *testing.T) {
	f := newCPFixture(t)
	f.r.Options.ImageRegistryMirror, f.r.Options.ChartRegistryMirror, f.r.Options.RegistrySecret = "mirror.local/cache", "mirror.local/charts", "mirror-auth"
	f.secret(f.mgmtKube, "aep-system", "mirror-auth", corev1.SecretTypeDockerConfigJson, map[string][]byte{".dockerconfigjson": []byte(`{"auths":{}}`)})
	f.markTenantAndRun()
	f.seedCluster(3)
	f.expect("PrerequisitesReady", false, "aep-cp-kgateway-crds")
	pull, err := f.remote.get(f.ctx, "v1", "secrets", OpenChoreoNamespace, MirrorPullSecret)
	if err != nil || pull.Object["type"] != "kubernetes.io/dockerconfigjson" {
		t.Fatal(err)
	}
	if _, err := f.mgmt.get(f.ctx, "v1", "secrets", cpNS, "mirror-auth"); err != nil {
		t.Fatal("Flux source credential not copied into the tenant namespace", err)
	}
	if _, err := f.remote.get(f.ctx, "gateway.kgateway.dev/v1alpha1", "gatewayparameters", OpenChoreoNamespace, MirrorGatewayParameters); err != nil {
		t.Fatal(err)
	}
	ref, _, _ := unstructured.NestedMap(f.release("aep-cp-kgateway").Object, "spec", "values", "gatewayClassParametersRefs", "kgateway")
	if ref["namespace"] != OpenChoreoNamespace {
		t.Fatal(ref)
	}
	source, err := f.mgmt.get(f.ctx, "source.toolkit.fluxcd.io/v1", "ocirepositories", cpNS, "aep-cp-kgateway")
	if url, _, _ := unstructured.NestedString(source.Object, "spec", "url"); err != nil || !strings.HasPrefix(url, "oci://mirror.local/charts/") {
		t.Fatal(err, url)
	}
}

func TestControlPlaneTokenRenewalAndContractChange(t *testing.T) {
	f := newCPFixture(t)
	f.seedCluster(0)
	s := f.r.session(f.cp)
	if _, err := s.identity(f.ctx); err != nil {
		t.Fatal(err)
	}
	cred := func() *unstructured.Unstructured {
		u, err := f.mgmt.get(f.ctx, "v1", "secrets", "aep-system", "operator-access-cp-default")
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	revision, _ := controlPlaneContractRevision()
	if dataValue(t, cred(), "contract") != revision {
		t.Fatal("contract revision not recorded")
	}
	// An expiring token is renewed with the scoped identity, not the super-admin.
	u := cred()
	u.Object["data"].(map[string]any)["expires"] = encode(time.Now().Add(time.Hour).Format(time.RFC3339))
	u.Object["data"].(map[string]any)["contract"] = encode("stale")
	if _, err := f.mgmtDyn.Resource(gvr("v1", "secrets")).Namespace("aep-system").Update(f.ctx, u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.remoteDyn.ClearActions()
	if _, err := f.r.session(f.cp).identity(f.ctx); err != nil {
		t.Fatal(err)
	}
	expires, _ := time.Parse(time.RFC3339, dataValue(t, cred(), "expires"))
	if time.Until(expires) < 20*time.Hour || dataValue(t, cred(), "contract") != revision {
		t.Fatal("token not renewed or contract not reinstalled", expires)
	}
	reinstalled := false
	for _, a := range f.remoteDyn.Actions() {
		reinstalled = reinstalled || a.GetResource().Resource == "clusterroles"
	}
	if !reinstalled {
		t.Fatal("changed contract not reinstalled")
	}
	// A foreign credential is never used.
	u = cred()
	u.SetLabels(map[string]string{ControlPlaneLabel: "other"})
	if _, err := f.mgmtDyn.Resource(gvr("v1", "secrets")).Namespace("aep-system").Update(f.ctx, u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.session(f.cp).identity(f.ctx); err == nil || !strings.Contains(err.Error(), "ownership mismatch") {
		t.Fatal(err)
	}
}

func encode(s string) string {
	return secretObject("", "", map[string][]byte{"v": []byte(s)}).Object["data"].(M)["v"].(string)
}

func TestControlPlaneDeletion(t *testing.T) {
	org := organization()
	org.Spec.ControlPlaneRef = "default"
	f := newCPFixture(t, org)
	f.cp.Finalizers = []string{ControlPlaneFinalizer}
	if err := f.client.Update(f.ctx, f.cp); err != nil {
		t.Fatal(err)
	}
	owned := func(u *unstructured.Unstructured) *unstructured.Unstructured {
		u.SetLabels(map[string]string{ControlPlaneLabel: "cp-uid"})
		return u
	}
	for _, item := range []struct {
		resource string
		u        *unstructured.Unstructured
	}{
		{"helmreleases", obj(helmAPI, "HelmRelease", cpNS, "aep-cp-kgateway-crds", M{})},
		{"helmreleases", obj(helmAPI, "HelmRelease", cpNS, "aep-cp-kgateway", M{})},
		{"helmreleases", obj(helmAPI, "HelmRelease", cpNS, "aep-cp-openchoreo-control-plane", M{})},
		{"ocirepositories", obj("source.toolkit.fluxcd.io/v1", "OCIRepository", cpNS, "aep-cp-kgateway", M{})},
		{"ingresses", obj("networking.k8s.io/v1", "Ingress", cpNS, PublicIngress, M{})},
		{"ciliumclusterwidenetworkpolicies", obj("cilium.io/v2", "CiliumClusterwideNetworkPolicy", "", "aep-cp-default-root-egress", M{})},
		{"kubernetesnodeses", obj(appsAPI, "KubernetesNodes", cpNS, "aepcp-md0", M{})},
		{"kuberneteses", obj(appsAPI, "Kubernetes", cpNS, "aepcp", M{})},
		{"postgreses", obj(appsAPI, "Postgres", cpNS, "portal", M{})},
		{"secrets", obj("v1", "Secret", cpNS, "mirror-auth", nil)},
		{"tenants", obj(appsAPI, "Tenant", "tenant-root", "aepcp", M{})},
		{"secrets", obj("v1", "Secret", "aep-system", "operator-access-cp-default", nil)},
	} {
		f.create(f.mgmtDyn, item.resource, owned(item.u))
	}
	// An unowned object in the tenant namespace survives.
	f.create(f.mgmtDyn, "ingresses", obj("networking.k8s.io/v1", "Ingress", cpNS, "tenant-app", M{}))
	if err := f.client.Delete(f.ctx, f.cp); err != nil {
		t.Fatal(err)
	}
	blocked := func(want string) {
		t.Helper()
		res, err := f.reconcile()
		ready := meta.FindStatusCondition(f.cp.Status.Conditions, "Ready")
		if err != nil || res.RequeueAfter == 0 || f.cp.Status.Phase != "DeletionBlocked" || ready == nil || !strings.Contains(ready.Message, want) {
			t.Fatalf("res=%v err=%v status=%+v", res, err, f.cp.Status)
		}
	}
	blocked("set aep.aenix.io/confirm-delete=default")
	f.cp.Annotations = map[string]string{"aep.aenix.io/confirm-delete": "default"}
	if err := f.client.Update(f.ctx, f.cp); err != nil {
		t.Fatal(err)
	}
	blocked("Organization acme uses this control plane")
	if len(f.mgmtDyn.Actions()) != 0 && slices.ContainsFunc(f.mgmtDyn.Actions(), func(a ktesting.Action) bool { return a.GetVerb() == "delete" }) {
		t.Fatal("deleted while blocked")
	}
	if err := f.client.Delete(f.ctx, org); err != nil {
		t.Fatal(err)
	}
	var order []string
	step := func() bool {
		f.mgmtDyn.ClearActions()
		_, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range f.mgmtDyn.Actions() {
			if d, ok := a.(ktesting.DeleteAction); ok {
				order = append(order, a.GetResource().Resource+"/"+d.GetName())
			}
		}
		return f.client.Get(f.ctx, types.NamespacedName{Name: "default"}, f.cp) != nil
	}
	for i := 0; i < 20 && !step(); i++ {
		if c := meta.FindStatusCondition(f.cp.Status.Conditions, "Ready"); c != nil && strings.Contains(c.Message, "is kept") {
			break
		}
	}
	if _, err := f.mgmt.get(f.ctx, appsAPI, "postgreses", cpNS, "portal"); err != nil {
		t.Fatal("portal database deleted without the annotation", err)
	}
	if _, err := f.mgmt.get(f.ctx, appsAPI, "tenants", "tenant-root", "aepcp"); err != nil {
		t.Fatal("tenant deleted while it holds the kept database", err)
	}
	f.cp.Annotations[DeletePortalDatabaseAnnotation] = "true"
	if err := f.client.Update(f.ctx, f.cp); err != nil {
		t.Fatal(err)
	}
	gone := false
	for i := 0; i < 20 && !gone; i++ {
		gone = step()
	}
	if !gone {
		t.Fatal("finalizer not released", f.cp.Status)
	}
	want := []string{"helmreleases/aep-cp-openchoreo-control-plane", "helmreleases/aep-cp-kgateway", "helmreleases/aep-cp-kgateway-crds",
		"ocirepositories/aep-cp-kgateway", "ingresses/" + PublicIngress, "ciliumclusterwidenetworkpolicies/aep-cp-default-root-egress",
		"kubernetesnodeses/aepcp-md0", "kuberneteses/aepcp", "postgreses/portal", "secrets/mirror-auth", "tenants/aepcp", "secrets/operator-access-cp-default"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("deletion order\n got %v\nwant %v", order, want)
	}
	if _, err := f.mgmt.get(f.ctx, "networking.k8s.io/v1", "ingresses", cpNS, "tenant-app"); err != nil {
		t.Fatal("unowned object deleted", err)
	}
}

func TestControlPlaneDeletionOfUninstalledControlPlane(t *testing.T) {
	f := newCPFixture(t)
	if err := f.client.Delete(f.ctx, f.cp); err != nil {
		t.Fatal(err)
	}
	// Without the finalizer nothing was installed and deletion is immediate.
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(f.cp), f.cp); err == nil {
		t.Fatal("ControlPlane without finalizer survived deletion")
	}
}

func TestUsesControlPlane(t *testing.T) {
	o := organization()
	opts := Options{DefaultControlPlane: "default"}
	if !opts.usesControlPlane(o, "default") {
		t.Fatal("an Organization without reference resolves to the default control plane")
	}
	opts.CPSecret = "tenant-aep-cp/admin"
	if opts.usesControlPlane(o, "default") {
		t.Fatal("legacy kubeconfig flags keep Organizations on the legacy aep-cp")
	}
	o.Spec.ControlPlaneRef = "cp2"
	if !opts.usesControlPlane(o, "cp2") || opts.usesControlPlane(o, "default") {
		t.Fatal("reference")
	}
	o.Spec.ControlPlaneRef, o.Status.ControlPlaneRef = "", "cp2"
	if !opts.usesControlPlane(o, "cp2") {
		t.Fatal("accepted reference")
	}
}
