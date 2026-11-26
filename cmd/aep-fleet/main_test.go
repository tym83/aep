package main

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestBootstrapTokenIsRevocableAndShortLived(t *testing.T) {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap", UID: "unique"}}
	r := bootstrapTokenRequest(s)
	if r.Spec.BoundObjectRef == nil || r.Spec.BoundObjectRef.Kind != "Secret" || r.Spec.BoundObjectRef.Name != s.Name || r.Spec.BoundObjectRef.UID != s.UID || *r.Spec.ExpirationSeconds != 600 {
		t.Fatalf("unbound token: %+v", r.Spec)
	}
}
