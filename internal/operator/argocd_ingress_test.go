package operator

import (
	"testing"
)

func TestArgoTierIngressAndCertificate(t *testing.T) {
	o := argoOrg()
	opt := Options{ArgoCDHostTemplate: "argocd.{{.Tier}}.{{.Organization}}.example.org"}
	addons := tierAddons(o, opt, "nonprod")
	ingress, _ := addons["ingressNginx"].(M)
	if ingress["enabled"] != true || ingress["exposeMethod"] != "Proxied" || ingress["hosts"].([]any)[0] != "argocd.nonprod.acme.example.org" {
		t.Fatalf("nonprod ingress %v", addons)
	}
	if _, ok := tierAddons(o, opt, "prod")["ingressNginx"]; ok {
		t.Fatal("prod tier got an Argo CD ingress")
	}
	o.Spec.Tiers["nonprod"].ArgoCD.Enabled = false
	if _, ok := tierAddons(o, opt, "nonprod")["ingressNginx"]; ok {
		t.Fatal("ingress without Argo CD")
	}
	values := argoValues(argoOrg(), "argocd.nonprod.acme.example.org", "https://id.example", []string{"payments"})
	before := len(values["extraObjects"].([]any))
	argoTLS(values, "")
	if len(values["extraObjects"].([]any)) != before {
		t.Fatal("issuer without ACME server")
	}
	argoTLS(values, "https://acme.example/directory")
	ann := values["server"].(M)["ingress"].(M)["annotations"].(M)
	if ann["cert-manager.io/cluster-issuer"] != "aep-argocd-acme" {
		t.Fatalf("annotations %v", ann)
	}
	last := values["extraObjects"].([]any)[before].(M)
	if last["kind"] != "ClusterIssuer" || last["spec"].(M)["acme"].(M)["server"] != "https://acme.example/directory" {
		t.Fatalf("issuer %v", last)
	}
}
