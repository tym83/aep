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
// plane chart. Empty optional fields take the defaults of the ControlPlane API.
type ControlPlaneSettings struct {
	// Domain carries the console.<domain> and api.<domain> hosts.
	Domain string
	// Issuer is the OIDC issuer, a Keycloak realm URL.
	Issuer string
	// JWKSURL, AuthorizationURL, TokenURL and WellKnownURL default to the
	// Keycloak layout below Issuer.
	JWKSURL, AuthorizationURL, TokenURL, WellKnownURL string
	// UsernameClaim names the user in audit events and portal sessions.
	UsernameClaim string
	// CLIClientID and PortalClientID are the public CLI and confidential portal clients.
	CLIClientID, PortalClientID string
	// GatewayHost is the cluster gateway host that tier agents dial; empty
	// until it is known.
	GatewayHost string
	// PortalRepository overrides the aep-portal image repository.
	PortalRepository string
	// PortalTag is the aep-portal image tag, normally the operator version.
	PortalTag string
	// ReadAPIURL and ChangeAPIURL are the AEP APIs the portal calls; empty omits them.
	ReadAPIURL, ChangeAPIURL string
	// OIDCCA mounts the ConfigMap OIDCCAConfigMap as the portal's extra CA bundle.
	OIDCCA bool
}

// PortalDatabaseSecret holds the portal database user and password on aep-cp.
const PortalDatabaseSecret = "aep-portal-db"

// PortalDatabaseService is the selectorless aep-cp Service of the portal database.
const PortalDatabaseService = "aep-portal-db"

// OIDCCAConfigMap holds the private IdP CA bundle on aep-cp.
const OIDCCAConfigMap = "aep-oidc-ca"

const oidcCAPath = "/etc/aep-oidc-ca"

// PortalAuthProvider is the portal's sign-in provider (portal/app-config.production.yaml).
const PortalAuthProvider = "openchoreo-auth"

// systemBootstrapMappings are the chart's bootstrap role bindings that grant
// only system clients. Group bindings and the shared workload publisher are
// left out: Organizations bind their own groups and publishers.
func systemBootstrapMappings(portalClient string) []any {
	binding := func(name, role, client string) M {
		return M{"name": name, "kind": "ClusterAuthzRoleBinding", "system": true, "effect": "allow",
			"roleMappings": []any{M{"roleRef": M{"name": role, "kind": "ClusterAuthzRole"}}},
			"entitlement":  M{"claim": "client_id", "value": client}}
	}
	return []any{
		binding("backstage-catalog-reader-binding", "backstage-catalog-reader", portalClient),
		binding("finops-agent-binding", "finops-agent", "openchoreo-finops-agent"),
		binding("rca-agent-binding", "rca-agent", "openchoreo-rca-agent"),
		binding("observer-resource-reader-binding", "observer-resource-reader", "openchoreo-observer-resource-reader-client"),
	}
}

func orDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func env(name, value string) M { return M{"name": name, "value": value} }

func secretEnv(name, secret, key string) M {
	return M{"name": name, "valueFrom": M{"secretKeyRef": M{"name": secret, "key": key}}}
}

// portalEnv configures the AEP portal (portal/app-config.production.yaml). TLS
// verification stays on: a private IdP CA is added, never verification removed.
func portalEnv(s ControlPlaneSettings, console string) []any {
	portalClient := orDefault(s.PortalClientID, "openchoreo-backstage-client")
	out := []any{
		env("PORTAL_BASE_URL", console),
		env("KEYCLOAK_ISSUER", s.Issuer),
		env("KEYCLOAK_CLIENT_ID", portalClient),
		secretEnv("KEYCLOAK_CLIENT_SECRET", PortalSecret, "client-secret"),
		env("OPENCHOREO_CLIENT_ID", portalClient),
		secretEnv("OPENCHOREO_CLIENT_SECRET", PortalSecret, "client-secret"),
		secretEnv("AUTH_SESSION_SECRET", PortalSecret, "backend-secret"),
	}
	if s.ReadAPIURL != "" {
		out = append(out, env("AEP_READ_API_URL", s.ReadAPIURL))
	}
	if s.ChangeAPIURL != "" {
		out = append(out, env("AEP_CHANGE_API_URL", s.ChangeAPIURL))
	}
	out = append(out,
		env("POSTGRES_HOST", PortalDatabaseService),
		env("POSTGRES_PORT", "5432"),
		secretEnv("POSTGRES_USER", PortalDatabaseSecret, "user"),
		secretEnv("POSTGRES_PASSWORD", PortalDatabaseSecret, "password"),
		env("APP_CONFIG_backend_database_pluginDivisionMode", "schema"),
		env("APP_CONFIG_backend_database_connection_database", "portal"))
	if s.OIDCCA {
		out = append(out, env("NODE_EXTRA_CA_CERTS", oidcCAPath+"/ca.crt"))
	}
	return out
}

// ControlPlaneValues is the control plane chart configuration. It is pure, so
// release inventory renders exactly what reconciliation would install.
func ControlPlaneValues(s ControlPlaneSettings) M {
	console, api := "console."+s.Domain, "api."+s.Domain
	endpoints := s.Issuer + "/protocol/openid-connect"
	portalClient := orDefault(s.PortalClientID, "openchoreo-backstage-client")
	dnsNames := []any{"cluster-gateway.openchoreo-control-plane.svc", "cluster-gateway.openchoreo-control-plane.svc.cluster.local"}
	if s.GatewayHost != "" {
		dnsNames = append(dnsNames, s.GatewayHost)
	}
	backstage := M{"secretName": PortalSecret, "baseUrl": "https://" + console, "http": M{"hostnames": []any{console}},
		"image": M{"repository": orDefault(s.PortalRepository, PortalImage), "tag": s.PortalTag},
		"auth":  M{"clientId": portalClient, "redirectUrls": []any{"https://" + console + "/api/auth/" + PortalAuthProvider + "/handler/frame"}},
		// The chart starts Backstage with its own config files; the portal's
		// production config must come last so its database and auth settings win.
		"extraArgs": []any{"--config", "app-config.production.yaml"},
		"extraEnv":  portalEnv(s, "https://"+console)}
	if s.OIDCCA {
		backstage["extraVolumes"] = []any{M{"name": OIDCCAConfigMap, "configMap": M{"name": OIDCCAConfigMap}}}
		backstage["extraVolumeMounts"] = []any{M{"name": OIDCCAConfigMap, "mountPath": oidcCAPath, "readOnly": true}}
	}
	return M{
		// TLS ends at the management ingress in front of the gateway.
		"gateway": M{"tls": M{"enabled": false}},
		"security": M{"oidc": M{"issuer": s.Issuer,
			"wellKnownEndpoint":            orDefault(s.WellKnownURL, s.Issuer+"/.well-known/openid-configuration"),
			"jwksUrl":                      orDefault(s.JWKSURL, endpoints+"/certs"),
			"authorizationUrl":             orDefault(s.AuthorizationURL, endpoints+"/auth"),
			"tokenUrl":                     orDefault(s.TokenURL, endpoints+"/token"),
			"jwksUrlTlsInsecureSkipVerify": false, "uidResolverTlsInsecureSkipVerify": false,
			"externalClients": []any{M{"name": "cli", "client_id": orDefault(s.CLIClientID, "openchoreo-cli"), "scopes": []any{"openid", "profile", "email"}}}}},
		"openchoreoApi": M{"http": M{"hostnames": []any{api}}, "config": M{"server": M{"publicUrl": "https://" + api},
			"security": M{
				"authentication": M{"jwt": M{"jwks": M{"skip_tls_verify": false}}},
				"subjects":       M{"user": M{"mechanisms": M{"jwt": M{"readable_id_claim": orDefault(s.UsernameClaim, "preferred_username")}}}},
				"authorization":  M{"bootstrap": M{"mappings": systemBootstrapMappings(portalClient)}}}}},
		"backstage":      backstage,
		"clusterGateway": M{"service": M{"type": "LoadBalancer"}, "tls": M{"dnsNames": dnsNames}},
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
