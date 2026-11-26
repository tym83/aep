// aep-fleet performs operator-authorized OCM bootstrap from an administrator's
// workstation. The managed cluster subsequently initiates every connection.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	api "github.com/tym83/aep/api/v1alpha1"
	"github.com/tym83/aep/internal/change"
	"github.com/tym83/aep/internal/operator"
	"github.com/tym83/aep/internal/registrymirror"
	inventory "github.com/tym83/aep/release"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	configapi "k8s.io/client-go/tools/clientcmd/api"
	"os"
	"os/exec"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stdout, "Usage: aep-fleet <command>\n\nCommands:\n  join   Bootstrap an approved cluster\n  stage  Stage pinned charts for offline delivery")
		return nil
	}
	if len(args) > 0 && args[0] == "stage" {
		return stage(args[1:])
	}
	if len(args) > 0 && args[0] == "join" {
		args = args[1:]
	} else if len(args) == 0 || args[0] != "-h" && args[0] != "--help" {
		return fmt.Errorf("usage: aep-fleet <join|stage> -h")
	}
	fs := flag.NewFlagSet("aep-fleet join", flag.ContinueOnError)
	var management, hub, target, ns, name, mirror string
	var execute bool
	fs.StringVar(&mirror, "registry-mirror", "", "Air-gap registry prefix; retains upstream host paths")
	fs.StringVar(&management, "management-config", "", "Management admin config")
	fs.StringVar(&hub, "hub-config", "", "OCM hub config; default management")
	fs.StringVar(&target, "target-config", "", "Target cluster config, required for execute")
	fs.StringVar(&ns, "namespace", "", "ExternalCluster namespace")
	fs.StringVar(&name, "cluster", "", "ExternalCluster name")
	fs.BoolVar(&execute, "execute", false, "Install klusterlet; otherwise print Helm values containing a short-lived credential")
	if e := fs.Parse(args); e != nil {
		if e == flag.ErrHelp {
			return nil
		}
		return e
	}
	if management == "" || ns == "" || name == "" || execute && target == "" {
		return fmt.Errorf("management-config, namespace, cluster and (for execute) target-config required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg, e := clientcmd.BuildConfigFromFlags("", management)
	if e != nil {
		return e
	}
	scheme := runtime.NewScheme()
	if e = api.AddToScheme(scheme); e != nil {
		return e
	}
	c, e := client.New(cfg, client.Options{Scheme: scheme})
	if e != nil {
		return e
	}
	ec := &api.ExternalCluster{}
	if e = c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, ec); e != nil {
		return e
	}
	if e = change.VerifyExternalApproval(ctx, c, ec); e != nil {
		return e
	}
	if meta.IsStatusConditionTrue(ec.Status.Conditions, "Joined") {
		return fmt.Errorf("cluster already joined; bootstrap reissuance requires platform recovery")
	}
	if !ec.DeletionTimestamp.IsZero() || ec.Spec.Suspended {
		return fmt.Errorf("cluster is deleting")
	}
	if hub != "" {
		cfg, e = clientcmd.BuildConfigFromFlags("", hub)
		if e != nil {
			return e
		}
	}
	if cfg.Insecure {
		return fmt.Errorf("hub TLS verification is required")
	}
	k, e := kubernetes.NewForConfig(cfg)
	if e != nil {
		return e
	}
	id := operator.FleetID(ec)
	hubClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	mc, err := hubClient.Resource(schema.GroupVersionResource{Group: "cluster.open-cluster-management.io", Version: "v1", Resource: "managedclusters"}).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return err
	}
	accepted, _, _ := unstructured.NestedBool(mc.Object, "spec", "hubAcceptsClient")
	if !accepted {
		return fmt.Errorf("hub has not accepted this assignment")
	}
	conditions, _, _ := unstructured.NestedSlice(mc.Object, "status", "conditions")
	for _, raw := range conditions {
		c, _ := raw.(map[string]any)
		if c["type"] == "ManagedClusterJoined" && c["status"] == "True" {
			return fmt.Errorf("cluster already joined; bootstrap reissuance requires platform recovery")
		}
	}
	saName := "bootstrap-" + id
	hubNS := "aep-" + ec.Spec.Organization
	// The registrar creates the namespace and accepted ManagedCluster first.
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: hubNS, Labels: map[string]string{"aep.aenix.io/external-cluster-uid": string(ec.UID)}}}
	old, e := k.CoreV1().ServiceAccounts(hubNS).Create(ctx, sa, metav1.CreateOptions{})
	if errors.IsAlreadyExists(e) {
		old, e = k.CoreV1().ServiceAccounts(hubNS).Get(ctx, saName, metav1.GetOptions{})
	}
	if e != nil {
		return e
	}
	if old.Labels["aep.aenix.io/external-cluster-uid"] != string(ec.UID) {
		return fmt.Errorf("bootstrap account ownership mismatch")
	}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: saName}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"certificatesigningrequests"}, Verbs: []string{"create", "get", "list", "watch"}},
		{APIGroups: []string{"cluster.open-cluster-management.io"}, Resources: []string{"managedclusters"}, ResourceNames: []string{id}, Verbs: []string{"get", "list", "watch"}},
	}}
	// Unique immutable bootstrap RBAC is created once; never overwrite preexisting permissions.
	if _, e = k.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); e != nil {
		if !errors.IsAlreadyExists(e) {
			return e
		}
		existing, err := k.RbacV1().ClusterRoles().Get(ctx, saName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(existing.Rules, role.Rules) {
			return fmt.Errorf("bootstrap role differs from expected permissions")
		}
	}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: saName}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: saName}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: saName, Namespace: hubNS}}}
	if _, e = k.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{}); e != nil {
		if !errors.IsAlreadyExists(e) {
			return e
		}
		existing, err := k.RbacV1().ClusterRoleBindings().Get(ctx, saName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(existing.Subjects, binding.Subjects) || existing.RoleRef != binding.RoleRef {
			return fmt.Errorf("bootstrap role binding ownership mismatch")
		}
	}

	publicRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: "kube-public"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}}}
	if _, e = k.RbacV1().Roles("kube-public").Create(ctx, publicRole, metav1.CreateOptions{}); errors.IsAlreadyExists(e) {
		existing, err := k.RbacV1().Roles("kube-public").Get(ctx, saName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(existing.Rules, publicRole.Rules) {
			return fmt.Errorf("bootstrap public role differs")
		}
	} else if e != nil {
		return e
	}
	publicBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: "kube-public"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: saName}, Subjects: binding.Subjects}
	if _, e = k.RbacV1().RoleBindings("kube-public").Create(ctx, publicBinding, metav1.CreateOptions{}); errors.IsAlreadyExists(e) {
		existing, err := k.RbacV1().RoleBindings("kube-public").Get(ctx, saName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(existing.Subjects, publicBinding.Subjects) || existing.RoleRef != publicBinding.RoleRef {
			return fmt.Errorf("bootstrap public binding differs")
		}
	} else if e != nil {
		return e
	}
	secret, e := k.CoreV1().Secrets(hubNS).Create(ctx, &corev1.Secret{ObjectMeta: sa.ObjectMeta}, metav1.CreateOptions{})
	if errors.IsAlreadyExists(e) {
		secret, e = k.CoreV1().Secrets(hubNS).Get(ctx, saName, metav1.GetOptions{})
	}
	if e != nil {
		return e
	}
	if secret.Labels["aep.aenix.io/external-cluster-uid"] != string(ec.UID) {
		return fmt.Errorf("bootstrap secret ownership mismatch")
	}
	token, e := k.CoreV1().ServiceAccounts(hubNS).CreateToken(ctx, saName, bootstrapTokenRequest(secret), metav1.CreateOptions{})
	if e != nil {
		return e
	}
	if token.Status.ExpirationTimestamp.After(time.Now().Add(15 * time.Minute)) {
		return fmt.Errorf("hub returned token lifetime greater than 15 minutes")
	}
	ca := cfg.CAData
	if len(ca) == 0 && cfg.CAFile != "" {
		ca, e = os.ReadFile(cfg.CAFile)
		if e != nil {
			return e
		}
	}
	kc := configapi.NewConfig()
	kc.Clusters["hub"] = &configapi.Cluster{Server: cfg.Host, CertificateAuthorityData: ca, TLSServerName: cfg.ServerName}
	kc.AuthInfos["bootstrap"] = &configapi.AuthInfo{Token: token.Status.Token}
	kc.Contexts["hub"] = &configapi.Context{Cluster: "hub", AuthInfo: "bootstrap"}
	kc.CurrentContext = "hub"
	data, e := clientcmd.Write(*kc)
	if e != nil {
		return e
	}
	values := map[string]any{"bootstrapHubKubeConfig": string(data), "klusterlet": map[string]any{"clusterName": id, "mode": "Singleton"}, "podSecurityContext": map[string]any{"runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}}
	overrides := map[string]any{}
	for key, image := range map[string]string{"registrationImage": "registration", "workImage": "work", "operatorImage": "registration-operator"} {
		pin, err := inventory.Image("quay.io/open-cluster-management/" + image + ":v1.4.0")
		if err != nil {
			return err
		}
		overrides[key] = registrymirror.Rewrite(pin, mirror)
	}
	values["images"] = map[string]any{"overrides": overrides}
	payload, e := json.MarshalIndent(values, "", "  ")
	if e != nil {
		return e
	}
	fmt.Fprintf(os.Stderr, "Bootstrap expires %s. Treat output as a credential. Chart: klusterlet 1.4.0.\n", token.Status.ExpirationTimestamp.Format(time.RFC3339))
	if !execute {
		fmt.Println(string(payload))
		return nil
	}
	chart, err := inventory.Lookup("chart", operator.KlusterletChartOCI, operator.KlusterletVersion)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "helm", "upgrade", "--install", "klusterlet", registrymirror.Rewrite(chart.Ref, mirror)+"@"+chart.Digest, "--namespace", "open-cluster-management", "--create-namespace", "--values", "-")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+target)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func bootstrapTokenRequest(secret *corev1.Secret) *authenticationv1.TokenRequest {
	ttl := int64(600)
	return &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &ttl, BoundObjectRef: &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Secret", Name: secret.Name, UID: secret.UID}}}
}
