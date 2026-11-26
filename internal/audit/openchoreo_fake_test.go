package audit

import (
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func fakeClient() kubernetes.Interface { return fake.NewClientset() }
