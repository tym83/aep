package bridge

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"slices"
	"strconv"

	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/kinds"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *Reconciler) credential(ctx context.Context, name, key string) ([]byte, error) {
	secret, e := r.Dynamic.Resource(TenantSecrets).Namespace(r.TenantNamespace).Get(ctx, name, metav1.GetOptions{})
	if e != nil {
		return nil, e
	}
	value, ok, e := unstructured.NestedString(secret.Object, "data", key)
	if e != nil || !ok || value == "" {
		return nil, fmt.Errorf("TenantSecret %s is missing key %s", name, key)
	}
	b, e := base64.StdEncoding.DecodeString(value)
	if e != nil {
		return nil, fmt.Errorf("TenantSecret %s has invalid encoded data", name)
	}
	return b, nil
}
func ConnectionIdentity(spec map[string]any, kind string) (string, string) {
	if kind == "Redis" {
		return "0", "default"
	}
	dbs, _ := spec["databases"].(map[string]any)
	users, _ := spec["users"].(map[string]any)
	keys := func(m map[string]any) []string {
		out := []string{}
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	d, u := keys(dbs), keys(users)
	if len(d) == 0 || len(u) == 0 {
		return "", ""
	}
	db, _ := dbs[d[0]].(map[string]any)
	roles, _ := db["roles"].(map[string]any)
	for _, role := range []string{"admin", "readonly"} {
		if names, ok := roles[role].([]any); ok && len(names) > 0 {
			namesCopy := []string{}
			for _, n := range names {
				if s, ok := n.(string); ok {
					namesCopy = append(namesCopy, s)
				}
			}
			slices.Sort(namesCopy)
			if len(namesCopy) > 0 {
				return d[0], namesCopy[0]
			}
		}
	}
	return d[0], u[0]
}
func (r *Reconciler) connect(ctx context.Context, c *api.ServiceClaim, m kinds.Mapping, name string) (bool, error) {
	svc := &corev1.Service{}
	if e := r.Management.Get(ctx, types.NamespacedName{Namespace: r.TenantNamespace, Name: m.Service(name)}, svc); e != nil {
		return false, e
	}
	ip := net.ParseIP(svc.Spec.ClusterIP)
	if ip == nil {
		return false, fmt.Errorf("management Service has no routable ClusterIP")
	}
	addressType := discoveryv1.AddressTypeIPv4
	if ip.To4() == nil {
		addressType = discoveryv1.AddressTypeIPv6
	}
	app, e := r.Dynamic.Resource(m.GVR()).Namespace(r.TenantNamespace).Get(ctx, name, metav1.GetOptions{})
	if e != nil {
		return false, e
	}
	if app.GetAnnotations()[Domain+"claim-uid"] != string(c.UID) || app.GetAnnotations()[Domain+"paused"] == "true" {
		return false, fmt.Errorf("application ownership or pause state changed")
	}
	spec, _, _ := unstructured.NestedMap(app.Object, "spec")
	database, username := ConnectionIdentity(spec, c.Spec.Kind)
	key := m.PasswordKey
	if key == "" {
		key = username
	}
	password, e := r.credential(ctx, m.Secret(name), key)
	if e != nil {
		return false, e
	}
	ca := []byte{}
	if m.CASuffix != "" {
		ca, e = r.credential(ctx, m.Release(name)+m.CASuffix, "ca.crt")
		if e != nil {
			return false, e
		}
	}
	host := c.Name + "." + c.Namespace + ".svc.cluster.local"
	uri := url.URL{Scheme: m.Scheme, Host: net.JoinHostPort(host, strconv.Itoa(int(m.Port))), User: url.UserPassword(username, string(password)), Path: "/" + database}
	if c.Spec.Kind == "Postgres" {
		uri.RawQuery = "sslmode=require"
	}
	owner := metav1.NewControllerRef(c, api.GroupVersion.WithKind("ServiceClaim"))
	// Garbage collection does not need permission to block deletion of the claim.
	blockDeletion := false
	owner.BlockOwnerDeletion = &blockDeletion
	metadata := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: map[string]string{Domain + "managed-by": "bridge"}, Annotations: map[string]string{Domain + "claim-uid": string(c.UID)}, OwnerReferences: []metav1.OwnerReference{*owner}}
	}
	secret := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metadata(c.Name + "-conn"), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"host": []byte(host), "port": []byte(strconv.Itoa(int(m.Port))), "database": []byte(database), "username": []byte(username), "password": password, "uri": []byte(uri.String()), "ca.crt": ca}}
	local := &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metadata(c.Name), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{{Name: "database", Port: m.Port, Protocol: corev1.ProtocolTCP}}}}
	portName, protocol, ready := "database", corev1.ProtocolTCP, true
	endpoint := &discoveryv1.EndpointSlice{TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSlice"}, ObjectMeta: metadata(c.Name), AddressType: addressType, Ports: []discoveryv1.EndpointPort{{Name: &portName, Protocol: &protocol, Port: &m.Port}}, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{svc.Spec.ClusterIP}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}}}
	endpoint.Labels[discoveryv1.LabelServiceName] = c.Name
	endpoint.Labels[discoveryv1.LabelManagedBy] = Manager
	drift := false
	for _, obj := range []client.Object{secret, local, endpoint} {
		old := obj.DeepCopyObject().(client.Object)
		e := r.App.Get(ctx, client.ObjectKeyFromObject(obj), old)
		if e != nil && !errors.IsNotFound(e) {
			return drift, e
		}
		if e == nil {
			if old.GetLabels()[Domain+"managed-by"] != "bridge" || old.GetAnnotations()[Domain+"claim-uid"] != string(c.UID) {
				return drift, fmt.Errorf("refusing to adopt %T %s", obj, obj.GetName())
			}
			if s, ok := old.(*corev1.Service); ok && (len(s.Spec.Selector) != 0 || s.Spec.Type != corev1.ServiceTypeClusterIP) {
				return drift, fmt.Errorf("local Service must be ClusterIP without a selector")
			}
			if oldEndpoint, ok := old.(*discoveryv1.EndpointSlice); ok {
				drift = !reflect.DeepEqual(oldEndpoint.Endpoints, endpoint.Endpoints) || oldEndpoint.AddressType != endpoint.AddressType
			}
		}
		if e := r.App.Patch(ctx, obj, client.Apply, client.FieldOwner(Manager), client.ForceOwnership); e != nil {
			return drift, e
		}
	}
	c.Status.Connection = &api.Connection{Host: host, Port: m.Port, Database: database, Username: username, SecretName: secret.Name}
	return drift, nil
}
