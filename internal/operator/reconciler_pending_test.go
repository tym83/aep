package operator

import (
	"context"
	"errors"
	"fmt"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"testing"
	"time"

	api "github.com/tym83/aep/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconcilePendingAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		apiFailure, statusFailure bool
	}{
		{name: "pending"},
		{name: "API failure", apiFailure: true},
		{name: "status failure", statusFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := api.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			o := organization()
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(o).WithObjects(o)
			statusErr := errors.New("status write failed")
			if tc.statusFailure {
				c = c.WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
						return statusErr
					},
				})
			}
			d := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			apiErr := apierrors.NewForbidden(schema.GroupResource{Group: "apps.cozystack.io", Resource: "tenants"}, "acme", errors.New("denied"))
			if tc.apiFailure {
				d.PrependReactor("get", "tenants", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, apiErr
				})
			}
			r := &Reconciler{Client: c.Build(), Management: &Cluster{Dynamic: d}}
			result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: o.Name}})
			if tc.apiFailure || tc.statusFailure {
				want := error(apiErr)
				if tc.statusFailure {
					want = statusErr
				}
				if !errors.Is(err, want) || result.RequeueAfter != 0 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				return
			}
			if err != nil || result.RequeueAfter != 30*time.Second {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
				t.Fatal(err)
			}
			hardening := meta.FindStatusCondition(o.Status.Conditions, "ControlPlaneHardened")
			if hardening == nil || hardening.Status != "False" || hardening.Reason != "InspectionFailed" {
				t.Fatalf("installation inspection missing while tenants pending: %+v", hardening)
			}
			condition := meta.FindStatusCondition(o.Status.Conditions, "TenantsReady")
			if o.Status.Phase != "Pending" || condition == nil || condition.Reason != "Pending" || condition.Status != "False" {
				t.Fatalf("unexpected status: %+v", o.Status)
			}
		})
	}
}

func TestRetryLimiterCapsFailures(t *testing.T) {
	limiter := retryLimiter()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme"}}
	var delay time.Duration
	for range 100 {
		delay = limiter.When(request)
		if delay > 2*time.Minute {
			t.Fatalf("retry delay %s exceeds cap", delay)
		}
	}
	if delay != 2*time.Minute {
		t.Fatalf("cap not reached: %s", delay)
	}
	limiter.Forget(request)
	if delay := limiter.When(request); delay != 5*time.Millisecond {
		t.Fatalf("delay did not reset: %s", delay)
	}
}

func TestDeletionPendingAndAPIFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint("API failure=", failure), func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := api.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			o := organization()
			o.Finalizers = []string{finalizer}
			if failure {
				o.Annotations = map[string]string{"aep.aenix.io/confirm-delete": o.Name}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(o).WithObjects(o).Build()
			// Without confirmation deletion is pending. With confirmation, fetching
			// the control plane kubeconfig must propagate the missing Secret error.
			r := &Reconciler{Client: c, Management: &Cluster{Kube: kubefake.NewClientset()}, Options: Options{CPSecret: "system/missing"}}
			result, err := r.finalize(ctx, o)
			if failure {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("expected API failure, got %v", err)
				}
			} else if err != nil || result.RequeueAfter != 30*time.Second {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

// A policy that blocks the data plane must be correctable: policies are
// delivered before the data plane, never gated on it.
func TestPoliciesPrecedeDataPlaneDelivery(t *testing.T) {
	steps := (&session{}).steps()
	policies, delivery := -1, -1
	for i, step := range steps {
		if step.name == "PoliciesReady" && policies < 0 {
			policies = i
		}
		if step.name == "DataPlanesConnected" && delivery < 0 {
			delivery = i
		}
	}
	if policies < 0 || delivery < 0 || policies > delivery {
		t.Fatalf("PoliciesReady at %d must precede DataPlanesConnected at %d", policies, delivery)
	}
}
