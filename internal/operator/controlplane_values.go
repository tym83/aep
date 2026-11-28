package operator

import (
	"github.com/tym83/aep/internal/registrymirror"
	inventory "github.com/tym83/aep/release"
)

// ControlPlaneChart is the OpenChoreo control plane chart installed on aep-cp.
const ControlPlaneChart = "openchoreo-control-plane"
const ControlPlaneChartVersion = "1.4.0-m.3"

// ExternalSecretsChart is the External Secrets Operator chart.
const ExternalSecretsChart = "external-secrets"
const ExternalSecretsChartVersion = "2.0.1"

// PortalImage is the AEP developer portal; it replaces the OpenChoreo UI image.
const PortalImage = "ghcr.io/tym83/aep-portal"

// PortalSecret holds the portal backend, OIDC client and Jenkins secrets.
const PortalSecret = "backstage-secrets"

// AuthzBootstrapImage runs the chart's authorization bootstrap hook Job.
const AuthzBootstrapImage = "registry.k8s.io/kubectl"

// controlPlaneImages are the control plane chart components with an
// image.repository/image.tag value pair, keyed by their upstream repository.
var controlPlaneImages = []struct {
	path []string
	ref  string
}{
	{[]string{"controllerManager", "image"}, "cr.openchoreo.dev/openchoreo/controller"},
	{[]string{"openchoreoApi", "image"}, "cr.openchoreo.dev/openchoreo/openchoreo-api"},
	{[]string{"clusterGateway", "image"}, "cr.openchoreo.dev/openchoreo/cluster-gateway"},
	{[]string{"eventForwarder", "image"}, "cr.openchoreo.dev/openchoreo/event-forwarder"},
}

var authzBootstrapImagePath = []string{"openchoreoApi", "config", "security", "authorization", "bootstrap", "image"}

// externalSecretsImages are the External Secrets components; all run one image.
var externalSecretsImages = [][]string{{"image"}, {"webhook", "image"}, {"certController", "image"}}

const externalSecretsImage = "ghcr.io/external-secrets/external-secrets"

// ControlPlaneSettings are the installation-specific inputs of the control
// plane chart.
type ControlPlaneSettings struct {
	// Domain carries the console.<domain> and api.<domain> hosts.
	Domain string
	// Issuer is the OIDC issuer, a Keycloak realm URL.
	Issuer string
	// GatewayHost is the cluster gateway host that tier agents dial.
	GatewayHost string
	// PortalTag is the aep-portal image tag, normally the operator version.
	PortalTag string
}

// ControlPlaneValues is the control plane chart configuration. It is pure, so
// release inventory renders exactly what reconciliation would install.
func ControlPlaneValues(s ControlPlaneSettings) M {
	console, api := "console."+s.Domain, "api."+s.Domain
	endpoints := s.Issuer + "/protocol/openid-connect"
	return M{
		// TLS ends at the management ingress in front of the gateway.
		"gateway": M{"tls": M{"enabled": false}},
		"security": M{"oidc": M{"issuer": s.Issuer, "jwksUrl": endpoints + "/certs",
			"authorizationUrl": endpoints + "/auth", "tokenUrl": endpoints + "/token"}},
		"openchoreoApi": M{"http": M{"hostnames": []any{api}}, "config": M{"server": M{"publicUrl": "https://" + api}}},
		"backstage": M{"secretName": PortalSecret, "baseUrl": "https://" + console, "http": M{"hostnames": []any{console}},
			"image": M{"repository": PortalImage, "tag": s.PortalTag}},
		"clusterGateway": M{"service": M{"type": "LoadBalancer"}, "tls": M{"dnsNames": []any{
			"cluster-gateway.openchoreo-control-plane.svc", "cluster-gateway.openchoreo-control-plane.svc.cluster.local", s.GatewayHost}}},
	}
}

// inventoryControlPlane holds representative values: hosts do not affect
// image selection, but the chart rejects placeholder .invalid domains.
var inventoryControlPlane = ControlPlaneSettings{Domain: "inventory.example", Issuer: "https://id.inventory.example/realms/inventory",
	GatewayHost: "cluster-gateway.inventory.example", PortalTag: "@build"}

func pinControlPlane(values M) M {
	for _, image := range controlPlaneImages {
		a := lookupImage(image.ref)
		setMap(values, append(append([]string{}, image.path...), "tag"), a.Version+"@"+a.Digest)
	}
	a := lookupImage(AuthzBootstrapImage)
	setMap(values, authzBootstrapImagePath, a.Ref+":"+a.Version+"@"+a.Digest)
	return values
}

// mirrorControlPlane sets each repository explicitly. The chart's
// global.imageRegistry replaces the registry host of every image, which would
// move aep-portal under the OpenChoreo registry path, and it does not apply to
// the bootstrap hook image at all.
func mirrorControlPlane(mirror string, values M) M {
	for _, image := range controlPlaneImages {
		setMap(values, append(append([]string{}, image.path...), "repository"), registrymirror.Rewrite(image.ref, mirror))
	}
	if repo, ok := nested(values, "backstage", "image", "repository"); ok {
		setMap(values, []string{"backstage", "image", "repository"}, registrymirror.Rewrite(repo, mirror))
	}
	if bootstrap, ok := nested(values, authzBootstrapImagePath...); ok {
		setMap(values, authzBootstrapImagePath, registrymirror.Rewrite(bootstrap, mirror))
	}
	return values
}

func pinExternalSecrets(values M) M {
	a := lookupImage(externalSecretsImage)
	for _, path := range externalSecretsImages {
		setMap(values, append(append([]string{}, path...), "tag"), a.Version+"@"+a.Digest)
	}
	return values
}

// lookupImage returns the single inventory pin of an upstream repository.
func lookupImage(ref string) inventory.Artifact {
	a, err := inventory.Lookup("image", ref, "")
	if err != nil {
		panic(err) // Embedded inventory is validated by release checks.
	}
	return a
}

func nested(values M, path ...string) (string, bool) {
	var cur any = values
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur = m[key]
	}
	s, ok := cur.(string)
	return s, ok && s != ""
}
