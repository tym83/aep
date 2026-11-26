# Install the AEP management services

Run these commands from the repository root with kubectl and Helm pointed at the
**Cozystack management cluster**. This guide installs the organization operator,
its managed bridges, the change service and the read API. OpenChoreo runs in an
existing, separate control plane; this installation does not install it.

## Prerequisites and installation values

The management cluster needs Cozystack Tenant/Kubernetes/KubernetesNodes APIs,
Kamaji, Flux source/Helm controllers and Cilium. Prepare the existing OpenChoreo
control plane and its cluster gateway as described in [operator.md](operator.md).
For public API endpoints, install an ingress controller, configure DNS to its
address, and either install cert-manager with a ClusterIssuer or supply TLS
Secrets yourself. Images must be published and pullable by management nodes.

Choose the following values before installing:

| Value | Purpose / example |
|---|---|
| Management namespace | `aep-system`, used for all three Helm releases below |
| Operator and bridge images | Published immutable tags for `image` and `bridgeImage` in the operator chart |
| Change image tag | `image.tag`, shared by `ghcr.io/tym83/aep-change-api` and `ghcr.io/tym83/aep-change-controller` |
| Read API image | Full `image` reference, including a published immutable tag |
| OpenChoreo Kubernetes credentials | Operator `aepcpKubeconfigSecret`: management `namespace/name`, key `super-admin.svc` containing the control-plane kubeconfig |
| OpenChoreo cluster location | `aepcpTenantNamespace`, `aepcpCluster`, and `gatewayHostTemplate` matching the gateway certificate SANs |
| Keycloak realm issuer | Both APIs' `oidc.issuer`, e.g. `https://id.example.org/realms/aep` |
| JWKS URL | `oidc.jwksUrl`; empty uses `<issuer>/protocol/openid-connect/certs` |
| Token audience and claims | Both APIs' `oidc.audience`; `usernameClaim` and `groupsClaim` default to `preferred_username` and `groups` |
| OpenChoreo public API | Change `openchoreo.apiUrl`, e.g. `https://openchoreo.example.org` (base URL, not a Kubernetes API endpoint) |
| Client-credentials token URL | Change `openchoreo.tokenUrl`, e.g. `https://id.example.org/realms/aep/protocol/openid-connect/token`; each organization Secret can override it |
| Organizations and credentials | Change `organizations`, `orgNamespaceTemplate`, `openchoreo.secretNameTemplate`, `openchoreo.clientIdTemplate`, and an existing Secret per organization |
| Public API hostnames | Each chart's `ingress.host`, e.g. `change.example.org` and `read.example.org` |
| Ingress and certificates | Each chart's `ingress.className`, `ingress.tlsSecret`, optionally `ingress.clusterIssuer` |
| Compliance namespace | Read `complianceNamespace`, matching the operator release namespace |

Use a dedicated Keycloak realm and configure portal tokens with the intended API
audience and organization groups. Do not give those tokens a Kubernetes audience.
Remove broad OpenChoreo bootstrap permissions before onboarding users. Provision
one client-credentials identity per organization with only that organization's
required OpenChoreo release-binding permissions; only this identity should write
protected environments. AEP's Kubernetes RBAC does not grant OpenChoreo rights.

## 1. Operator and managed bridges

Create `operator-values.yaml` with your published image versions and locations:

```yaml
image: ghcr.io/tym83/aep-operator:REPLACE_TAG
bridgeImage: ghcr.io/tym83/aep-bridge:REPLACE_TAG
aepcpKubeconfigSecret: tenant-aep-cp/kubernetes-aepcp-admin-kubeconfig
aepcpTenantNamespace: tenant-aep-cp
aepcpCluster: aepcp
gatewayHostTemplate: 'cluster-gateway.{{.DashedIP}}.nip.io'
keycloak:
  enabled: false
  url: https://id.example.org
  realm: aep
  credentialsSecret: aep-system/keycloak-admin
```

If enabling operator-managed Keycloak groups, create `keycloak-admin` with
`username` and `password` keys and set `keycloak.enabled: true`. This integration
uses the Keycloak base URL and realm separately; it does not provision the realm,
federation mappings or change-controller OAuth clients.

```sh
helm upgrade --install aep-operator charts/aep-operator \
  --namespace aep-system --create-namespace -f operator-values.yaml
kubectl -n aep-system rollout status deployment/aep-operator
```

Create Organizations using the contract in [operator.md](operator.md), including
access groups, quotas, environments, tier settings and production image trust
keys. Publish the policy bundles and configure `policyBundleRepository` and
`kyvernoChartURL` if using different repositories. Wait for the Organization's
reported readiness conditions and observed generation.

The operator creates one bridge Deployment per physical tier, scopes its RBAC,
installs ServiceClaim CRDs on application clusters and rotates bridge credentials.
Do not separately install `charts/aep-bridge` for operator-managed tiers.
Organization parent namespaces such as `tenant-acme` must exist before installing
the change service's namespace-scoped RBAC.

## 2. Change service

Create an existing Secret in each organization's management namespace, preferably
through your secret manager. Do not put real credentials into Helm values or Git.
The default for organization `acme` is:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: aep-change-acme
  namespace: tenant-acme
type: Opaque
stringData:
  client-secret: REPLACE_WITH_SECRET
  client-id: aep-change-acme
  token-url: https://id.example.org/realms/aep/protocol/openid-connect/token
```

`client-secret` is required. Optional `client-id` and `token-url` override the
chart's client ID template and shared token URL. Secrets are read during use;
rotation does not require a controller restart. No static shared secret map is
supported by the binary. Secret names and namespaces are derived by substituting
`{org}` in the templates.

Create `change-values.yaml`:

```yaml
image:
  tag: REPLACE_TAG
organizations: [acme]
orgNamespaceTemplate: tenant-{org}
oidc:
  issuer: https://id.example.org/realms/aep
  audience: aep-portal
  jwksUrl: ""
  usernameClaim: preferred_username
  groupsClaim: groups
openchoreo:
  apiUrl: https://openchoreo.example.org
  tokenUrl: https://id.example.org/realms/aep/protocol/openid-connect/token
  clientIdTemplate: aep-change-{org}
  secretNameTemplate: aep-change-{org}
controller:
  leaseNamespace: aep-system
  approvalTTL: 72h
  maxAttempts: 8
ingress:
  enabled: true
  className: nginx
  host: change.example.org
  tlsSecret: change-public-tls
  clusterIssuer: letsencrypt
```

```sh
helm upgrade --install aep-change charts/aep-change \
  --namespace aep-system -f change-values.yaml
kubectl -n aep-system rollout status deployment/aep-change-api
kubectl -n aep-system rollout status deployment/aep-change-controller
```

The chart installs the ChangeRequest CRD from `crds/`. Helm does not upgrade or
remove CRDs automatically: review and apply `config/crd/aep.aenix.io_changerequests.yaml`
explicitly when upgrading its schema. The CRD is shared by all organizations.

Both services use their own in-cluster ServiceAccount; no kubeconfig is mounted.
The API listens on port 8080. Its chart requires an audience even though the binary
allows omitting that check. The controller watches ChangeRequests cluster-wide
because the binary has no namespace-filter flag. Write permissions and access to
the exact credential Secret are scoped to the configured organization namespaces.
Add every new organization to `organizations` and upgrade the release before using
its change API. The API has no Secret or Lease permissions.

Per-binding Leases live alongside ChangeRequests in the organization namespace.
The leader election Lease lives in `controller.leaseNamespace` (empty means the
release namespace); create a custom namespace beforehand. Leader election also
needs permission to create/patch Events there. The controller uses its pod hostname
as its unique instance ID. Run one change-service release per management cluster:
the binary's leader election Lease name is fixed.

For custom standard-change limits, create a ConfigMap in the release namespace
with key `policy.yaml` using the `kinds` format in
[change-service.md](change-service.md), then set `controller.policyConfigMap`.
Restart controller pods after changing mounted policy configuration.

TLS terminates at ingress by default. For TLS all the way to the change API, set
`api.tlsSecret` to an existing `kubernetes.io/tls` Secret in the release namespace;
both `tls.crt` and `tls.key` are mounted. Configure the ingress controller's HTTPS
upstream settings via `ingress.annotations` (for nginx,
`nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"`). Configure upstream
certificate verification according to your ingress controller's deployment policy.
For private OIDC/OpenChoreo CAs, set `caBundleConfigMap` to a release-namespace
ConfigMap containing `ca.crt`, a PEM trust bundle; it sets `SSL_CERT_FILE` in both
pods. Include all needed trust roots. The controller has **no insecure-skip-verify
flag** and always verifies OpenChoreo and token endpoint TLS. Restart affected pods
after rotating trust bundles or server certificates.

## 3. Read API

Create `read-values.yaml`:

```yaml
image: ghcr.io/tym83/aep-read-api:REPLACE_TAG
oidc:
  issuer: https://id.example.org/realms/aep
  audience: aep-portal
  jwksUrl: ""
  usernameClaim: preferred_username
  groupsClaim: groups
orgNamespaceTemplate: tenant-{org}
complianceNamespace: aep-system
consoleUrlTemplate: ""
ingress:
  enabled: true
  className: nginx
  host: read.example.org
  tlsSecret: read-public-tls
  clusterIssuer: letsencrypt
```

```sh
helm upgrade --install aep-read-api charts/aep-read-api \
  --namespace aep-system -f read-values.yaml
kubectl -n aep-system rollout status deployment/aep-read-api
```

The read API reads management state and compliance snapshots; it does not require
OpenChoreo OAuth client credentials. Keep namespace and OIDC mappings consistent
with the change API. Set `consoleUrlTemplate` only if console links are needed.
The read chart serves HTTP on port 8080 behind the TLS ingress. Its chart does not
currently expose the binary's direct TLS or custom CA options.

Both charts default to `ingress.enabled: false`. When enabled, `ingress.host` is
required. `className` may be empty to use the cluster default. To use an existing
TLS Secret, set `tlsSecret` and leave `clusterIssuer` empty; with cert-manager,
set both. An enabled ingress without `tlsSecret` provides HTTP only. Update the
portal's read/change backend endpoints to these hostnames.

## 4. Cost ledger and verification

This revision has no cost-ledger binary or Helm chart. Organization `costCenter`
is metadata, not an installed accounting service.

Before deployment, run the same local checks as CI:

```sh
make charts-lint
go build ./...
go vet ./...
go test ./...
```

`charts-lint` renders and lints both APIs with ingress disabled and enabled, tests
change TLS/custom namespace configuration, and checks the bundled ChangeRequest
CRD against its source. These checks do not contact a cluster. After installation,
verify DNS and certificates, API rejection of unauthenticated requests, authorized
organization reads, and a nonproduction change from creation through approval and
execution. Check controller logs for OAuth, RBAC and lease errors. Runtime
connectivity, IdP authorization and OpenChoreo permissions need this live check.
