package operator

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

func TestContractDriftReinstallsOnForbidden(t *testing.T) {
	o := organization()
	s := &session{o: o, r: &Reconciler{Management: &Cluster{Kube: kubefake.NewSimpleClientset()}, Options: Options{Namespace: "aep-system"}, Recorder: record.NewFakeRecorder(10)}, tiers: map[string]*Cluster{"nonprod": {}}}
	ctx := context.Background()
	secrets := s.r.Management.Kube.CoreV1().Secrets("aep-system")
	if _, err := secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "operator-access-acme-nonprod", Labels: map[string]string{OwnerLabel: string(o.UID)}},
		Data: map[string][]byte{"contract": []byte("current"), "kubeconfig": []byte("k")}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	other := errors.New("timeout")
	if err := s.contractDrift(ctx, "nonprod", other); err != other {
		t.Fatal("other errors must pass through", err)
	}
	if got, _ := secrets.Get(ctx, "operator-access-acme-nonprod", metav1.GetOptions{}); string(got.Data["contract"]) != "current" || s.tiers["nonprod"] == nil {
		t.Fatal("contract invalidated without a refusal")
	}
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "aep-mirror-pull", errors.New("no Role"))
	if err := s.contractDrift(ctx, "nonprod", denied); !apierrors.IsForbidden(err) {
		t.Fatal("refusal must stay visible", err)
	}
	got, _ := secrets.Get(ctx, "operator-access-acme-nonprod", metav1.GetOptions{})
	if len(got.Data["contract"]) != 0 || string(got.Data["kubeconfig"]) != "k" {
		t.Fatal("contract not invalidated or credential lost", got.Data)
	}
	if s.tiers["nonprod"] != nil {
		t.Fatal("cached tier client survived")
	}
}
