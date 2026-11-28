package operator

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestKgatewayValuesReferenceGatewayParameters(t *testing.T) {
	v := PullSecretValues("kgateway", M{})
	name, _, _ := unstructured.NestedString(v, "gatewayClassParametersRefs", "kgateway", "name")
	ns, _, _ := unstructured.NestedString(v, "gatewayClassParametersRefs", "kgateway", "namespace")
	if name != MirrorGatewayParameters || ns != "openchoreo-data-plane" {
		t.Fatal(v)
	}
}

func TestDeliverGatewayParameters(t *testing.T) {
	s := integrationSession(t)
	ctx := context.Background()
	c := s.tiers["nonprod"]
	if err := s.deliverGatewayParameters(ctx, c); err != nil {
		t.Fatal("no mirror must be a no-op", err)
	}
	if _, err := c.get(ctx, "gateway.kgateway.dev/v1alpha1", "gatewayparameters", "openchoreo-data-plane", MirrorGatewayParameters); !errors.IsNotFound(err) {
		t.Fatal("no mirror must not create GatewayParameters", err)
	}
	s.r.Options.ImageRegistryMirror, s.r.Options.RegistrySecret = "mirror.example/aep", "mirror-auth"
	if err := s.deliverGatewayParameters(ctx, c); err != nil {
		t.Fatal(err)
	}
	gp, err := c.get(ctx, "gateway.kgateway.dev/v1alpha1", "gatewayparameters", "openchoreo-data-plane", MirrorGatewayParameters)
	if err != nil {
		t.Fatal(err)
	}
	refs, _, _ := unstructured.NestedSlice(gp.Object, "spec", "kube", "podTemplate", "imagePullSecrets")
	if len(refs) != 1 || refs[0].(map[string]any)["name"] != MirrorPullSecret {
		t.Fatal(gp.Object)
	}

	// A fresh tier has no kgateway CRDs until the delivered charts install them.
	gvr := schema.GroupVersionResource{Group: "gateway.kgateway.dev", Version: "v1alpha1", Resource: "gatewayparameters"}
	fresh := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	fresh.PrependReactor("*", "gatewayparameters", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewNotFound(gvr.GroupResource(), "")
	})
	err = s.deliverGatewayParameters(ctx, &Cluster{Dynamic: fresh})
	if _, ok := err.(*pendingError); !ok {
		t.Fatal("missing CRDs must be pending", err)
	}
}
