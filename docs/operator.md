# Organization operator (stage 1)

The operator runs on the Cozystack management cluster. It provisions dedicated
application clusters and registers them with an OpenChoreo 1.4.0-m.3 control
plane (aep-cp). The supported path is an operator-managed aep-cp: a
[ControlPlane resource](#controlplane-resource) installs and manages it, and
Organizations use it by reference or as the default. An existing, hand-built
aep-cp can still be used through the legacy `--aepcp-*` flags. Organizations
never install aep-cp or change its Helm release.

## Installation

Build the operator image with `docker build -f Dockerfile.operator -t <image> .`
and the bridge image with the repository's default Dockerfile. Publish images to
your own registry before deployment; the chart's image names are placeholders.

With an operator-managed control plane (supported), install the operator with
a default ControlPlane name, then create that ControlPlane (see
[ControlPlane resource](#controlplane-resource) and [install.md](install.md)):

```sh
helm upgrade --install aep-operator charts/aep-operator \
  --namespace aep-system --create-namespace \
  --set image=<operator-image> --set bridgeImage=<bridge-image> \
  --set controlPlane.default=default
```

With an existing aep-cp (legacy), point the operator at it instead:

```sh
helm upgrade --install aep-operator charts/aep-operator \
  --namespace aep-system --create-namespace \
  --set image=<operator-image> --set bridgeImage=<bridge-image> \
  --set aepcpKubeconfigSecret=tenant-aep-cp/kubernetes-aepcp-admin-kubeconfig \
  --set aepcpTenantNamespace=tenant-aep-cp --set aepcpCluster=aepcp
```

Cozystack's aggregated Tenant/Kubernetes/KubernetesNodes APIs, Kamaji, Flux OCI
sources and Helm controllers, and Cilium CCNP must already be installed. The
operator namespace must exist. On a managed control plane the operator creates
its own scoped identity. For a legacy aep-cp, supply credentials with permission
to manage organization namespaces, OpenChoreo objects and admission policies,
read gateway Deployments/Secrets, and create SelfSubjectReviews. Its Kubernetes
identity must be dedicated to the operator, distinct from OpenChoreo's API
identity and users.

The operator checks installation-wide `ClusterAuthzRoleBindings` before tenant
provisioning on every Organization reconciliation, including startup reconciliation.
`ControlPlaneHardened` reports False with the sorted names of remaining bindings
for any `groups` claim, `client_id=service_mcp_client`, or the shared
`workload-publisher-binding`. Failed inspection also reports False. Ready requires
this condition to be True. Invalid Organizations still poll every minute; pending
ones retry with their normal provisioning cadence/backoff. On a legacy aep-cp at
least one Organization must exist for this reconciliation-based installation
check to run. A managed ControlPlane runs the check itself (`Hardened`), also
without Organizations, and its Organizations report that result as
`ControlPlaneHardened` instead of inspecting the installation again.

By default it deletes matching objects only when `app.kubernetes.io/managed-by=Helm`
and either `app.kubernetes.io/instance=openchoreo-control-plane` or
`release=openchoreo-control-plane` are present. Any `aep.aenix.io/*` label protects
an object from deletion. Unmanaged offenders remain visible for manual review.
Allow-listed client IDs are exempt (only for a `client_id` claim).
Each successful deletion emits a `BootstrapBindingPruned` Event on the triggering
Organization. The control-plane credentials need list/delete permission on
`clusterauthzrolebindings`. Helm upgrades may recreate bootstrap bindings; the
operator removes them again on reconciliation. This is eventual cleanup, so verify
`ControlPlaneHardened=True` before onboarding users and after upgrades.
With `--prune-bootstrap-bindings=false`, the operator only reports this condition.
The chart exposes `pruneBootstrapBindings` and `bootstrapBindingSystemClients`
values for these flags.

Each organization receives an owned `aep-workload-publisher` AuthzRoleBinding in
its namespace, mapping `client_id=aep-<org>-publisher` to the existing
`ClusterAuthzRole/workload-publisher`. Provision that distinct IdP client and its
credentials separately; the operator does not create publisher clients in the IdP.

The change controller acts in protected environments through its own identity.
The operator creates `AuthzRole/aep-change-executor` (read access plus create,
update and delete of release bindings in every environment of the organization)
and binds it to `client_id=aep-change-<org>`, the change service's default
client ID template. Provision this client in the IdP and store its credentials
in the `aep-change-<org>` Secret; no user group receives these rights in
protected environments.

Configure Keycloak as OIDC IdP
and emit the `groups` claim without a Kubernetes audience in portal tokens.
The isolated stand realm was `aepdev`; this operator never assumes `cozy`.

## Organization contract

```yaml
apiVersion: aep.aenix.io/v1alpha1
kind: Organization
metadata:
  name: acme
spec:
  displayName: ACME Payments
  profile: standard
  costCenter: CC-4711
  quotas: {cpu: "64", memory: 128Gi, storage: 2Ti, gpu: "0"}
  environments:
    - {name: dev, tier: nonprod, changeModel: none}
    - {name: stage, tier: nonprod, changeModel: none}
    - {name: prod, tier: prod, changeModel: required}
  tiers:
    nonprod:
      cluster:
        version: v1.35
        nodeGroups:
          md0: {minReplicas: 2, maxReplicas: 3, instanceType: u1.large}
      services: {allowedKinds: [Postgres, Redis, Kafka], maxPreset: u1.large}
    prod:
      cluster:
        version: v1.35
        nodeGroups:
          md0: {minReplicas: 3, maxReplicas: 4, instanceType: u1.large}
      services: {allowedKinds: [Postgres, Redis], maxPreset: u1.large}
  access:
    developer: [AD-ACME-Dev]
    platform: [AD-ACME-Platform]
    approver: [AD-ACME-CAB]
    admin: [AD-ACME-Admins]
    auditor: [AD-ACME-Audit]
```

Names are lowercase alphanumeric, up to 32 characters; `aep`, `root`, `cozy`,
`system` are reserved. Ownership uses the Organization **UID**, not its name.
Existing objects without matching ownership are never adopted, including the
manually created stand tenants. Profile changes require an explicit migration.
`small` creates only the nonprod physical tier, shared by logical nonprod/prod environments;
`regulated` reports `InvalidSpec` / not implemented. Only `md0` is supported.

`quotas` means the **total user budget**. Standard divides it equally across two
tiers, with indivisible remainders in nonprod; small assigns it to one tier.
Each tier receives another 20 CPU limit and 8Gi memory for its control plane.
The parent receives the sum, including overhead. Cozystack converts CPU limit to
requests at 10:1: user CPU 16 becomes tier CPU 36, giving 3.6 CPU requests.
Storage/GPU are split without overhead. Cost center and display name are metadata
for future accounting integrations, not billing implementation.

## Reconciliation

1. Create the parent Tenant in `tenant-root`, wait for observed Ready, then create
   and wait for each child sequentially. Gateway, ingress, etcd, monitoring and
   seaweedfs are disabled; host is empty. Install the root etcd/ingress workaround
   **before** waiting for clusters, because etcd connectivity is a prerequisite.
2. Create `Kubernetes/apps` and `KubernetesNodes/apps-md0` in each tier namespace,
   using replicated storage, cert-manager and Gateway API addons. Check Kamaji's
   version status, current addon HelmRelease conditions, and at least md0's minimum
   Ready nodes. Application Ready alone is insufficient. After ten minutes with a
   Ready control plane, zero nodes and no bootstrap token, force the base release.
   Also retry addons reporting `context deadline exceeded`. Record Events and a
   bounded remediation history; throttle each release to one remediation/10 min.
3. Find exactly one mirrored `cluster-gateway-external` Service in the configured
   aep-cp tenant. Verify its VM selector and render the egress/ingress pair using
   TCP 8443's numeric targetPort. Reconcile it again on each poll, including port
   changes. Verify the hostname against the gateway Deployment's mounted server
   certificate. If missing, report the exact name to add to
   `clusterGateway.tls.dnsNames`; reconcile the aep-cp chart and restart its gateway.
4. Bootstrap a tier-local `aep-system/aep-operator` SA, scoped RBAC, the embedded
   ServiceClaim CRD, bridge RBAC/admission and agent ClusterRole. Only bootstrap
   uses `super-admin.svc`. Subsequent operations and token renewals use the SA.
   The credential Secret records the contract revision; when an upgraded
   operator ships a different contract, it reinstalls it once with
   `super-admin.svc`.
   Its ClusterRole only lists Nodes and ServiceClaims; namespace Roles handle
   data-plane Secrets/ConfigMaps, listing kube-system bootstrap tokens and issuing
   tokens for the two named SAs. Credentials remain on management in owned Secrets.
5. Generate one ECDSA P-256 CA per tier, valid three years. Store it in
   `aep-system/agent-ca-<org>-<tier>` and copy cert+key to
   `openchoreo-data-plane/aep-agent-ca`. Copy the aep-cp gateway CA **certificate**
   into `cluster-gateway-ca` ConfigMap. Install OCI sources and remote HelmReleases
   for kgateway-crds/ kgateway v2.3.11 and openchoreo-data-plane 1.4.0-m.3 (into
   `openchoreo-data-plane`) and strimzi-kafka-operator 0.50.0 (into `aep-kafka`,
   with CRDs replaced on upgrade), with explicit dependencies, Cozystack sharding/target labels and the requested
   `super-admin.svc` kubeconfig reference. Agent certificates use the persistent
   CA; plane IDs are `<org>-<tier>`. Flux release readiness and actual DataPlane
   `status.agentConnection.connected` are both required.
6. Create the labeled organization namespace, namespaced DataPlanes, Environments
   and ordered default DeploymentPipeline on aep-cp. Its root must be nonprod.
   Do not create a Project. Generate five namespaced roles/bindings, including
   all four release-binding types with exact nonprod CEL restrictions. Platform
   object mutation is denied to each group. Install admission rules before granting
   access: delegated admin roles cannot expand the action allowlist or remove the
   nonprod constraints, reference ClusterAuthzRoles, or modify reserved `aep-*`
   roles. User access is granted only after `PoliciesReady`. Cascading Project/Component/Resource deletion currently requires platform
   decommission rather than permitting potentially destructive user deletion.
7. After `DataPlanesConnected`, deliver Kyverno and baseline/tier charts through
   remote Flux HelmReleases and wait for `PoliciesReady`. Production requires
   `spec.policy.cosignPublicKey`; its image verification always enforces. Independent
   policy maintenance expires exceptions every minute and collects compliance
   every five minutes. See [policies](policies.md) for configuration and publication.
8. Run a bridge Deployment per tier on management, using only a RoleBinding in
   that tier's namespace. Mount a token Secret as a directory (never subPath),
   request 24-hour bridge tokens and renew with 12 hours remaining. Honor the
   actual API-returned expiration. Set allowed kinds and maxPreset from the tier; propagate backupPolicy as a
   managed-application annotation for subsequent Backups Plan integration.
   Both tiers retain managed services. Credential renewal runs independently of
   provisioning gates while the Organization is active.
9. Optionally create five Keycloak groups in the existing configured realm. Group
   creation is idempotent. **TODO:** map `spec.access` external groups through the
   deployment's federation provider; no generic AD/Entra mapping is assumed.

The eight conditions carry observedGeneration; errors return through the
controller workqueue's exponential backoff. Successful organizations poll every
minute for external drift and connection changes. Reconciliation never rolls back
completed resources. aep-cp objects are written through the operator's Kubernetes
client, not delegated user credentials. Cluster changes and source outages do not
justify transferring ownership to pre-existing objects.

## Flags

| Flag | Default / meaning |
|---|---|
| `--mgmt-kubeconfig` | Empty: in-cluster credentials |
| `--namespace` | `aep-system`; chart uses its release namespace |
| `--aepcp-kubeconfig-secret` | Legacy aep-cp; required unless local file or `--default-control-plane`; management `namespace/name`, key `super-admin.svc` |
| `--aepcp-kubeconfig` | Local file; mutually exclusive with Secret reference |
| `--default-control-plane` | Empty; `ControlPlane` name for Organizations without `controlPlaneRef` (chart `controlPlane.default`) when no legacy kubeconfig is set; makes the legacy kubeconfig optional, see [Organizations on a ControlPlane](#organizations-on-a-controlplane) |
| `--aepcp-tenant-namespace` | Legacy aep-cp; `tenant-aep-cp` |
| `--aepcp-cluster` | Legacy aep-cp; `aepcp` |
| `--gateway-host-template` | `cluster-gateway.{{.DashedIP}}.nip.io`; also exposes `.ClusterIP`; used by legacy aep-cp and by ControlPlanes without `spec.clusterGateway.host` |
| `--tenant-egress-workaround` | `true`; false removes the owned workaround |
| `--bridge-image` | Required |
| `--prune-bootstrap-bindings` | `true`; false only reports unsafe installation bindings |
| `--bootstrap-binding-system-clients` | Comma-separated client IDs; defaults to `openchoreo-backstage-client,openchoreo-observer-resource-reader-client,openchoreo-rca-agent,openchoreo-finops-agent`; empty exempts none |
| `--keycloak-enabled` | `false` |
| `--keycloak-url` | Required when enabled; trusted HTTPS endpoint |
| `--keycloak-realm` | Required when enabled; existing isolated realm |
| `--keycloak-credentials-secret` | Required when enabled; management `namespace/name`, keys `username`, `password` |

Local execution still needs routes to the internal tier API Service addresses;
`super-admin.svc` does not work from an ordinary laptop without network access.
Use explicit local aep-cp kubeconfig/port-forward if needed. The management operator
is the supported continuous mode. Do not persist port-forward endpoints in tier
credentials used by bridge pods.

## ControlPlane resource

`ControlPlane.aep.aenix.io/v1alpha1` (cluster-scoped, short name `aepcp`)
declares an operator-managed aep-cp: Cozystack tenant and cluster, `domain`
(`console.<domain>`, `api.<domain>`), ingress with exactly one of
`clusterIssuer` or `tlsSecretRef`, OIDC, portal, cluster gateway host, content,
audit, backup and adoption. The CRD defaults the tenant (`tenant-root`/`aepcp`, namespace `tenant-aepcp`;
tenant names are alphanumeric because Cozystack joins them with dashes),
cluster (`aepcp`, `v1.35`, `replicated`, 3–4 × `u1.large`, 20Gi) and portal
database, and keeps the tenant and cluster names immutable. An invalid spec is
`Rejected` (`Ready=False`, reason `InvalidSpec`).

The operator installs a valid ControlPlane in this order, one condition per
step; a step waits for the previous ones:

| Condition | What the operator does |
|---|---|
| `TenantReady` | Tenant `spec.tenant.name` in `spec.tenant.parent`, all tenant services off, `spec.tenant.quotas` |
| `NetworkPathsReady` | With `--tenant-egress-workaround`, CCNP `aep-cp-<name>-root-egress` lets the tenant reach root etcd and ingress |
| `ClusterReady` | Kubernetes `spec.cluster.name` with cert-manager and Gateway API addons, KubernetesNodes `<cluster>-md0`; waits for the control plane, addons and `minReplicas` Ready nodes, with the same remediation as tier clusters |
| `AccessReady` | With the cluster's super-admin kubeconfig, only once or when the contract changes: ServiceAccount `kube-system/aep-operator`, a ClusterRole for OpenChoreo resources, namespaces, admission policies and RBAC, and a Role in `openchoreo-control-plane`; the token is stored in `<namespace>/operator-access-cp-<name>` and renewed before expiry |
| `PrerequisitesReady` | Mirror pull secret in `openchoreo-control-plane`, HelmReleases `aep-cp-kgateway-crds` and `aep-cp-kgateway`, GatewayParameters `aep-mirror-pull`. No External Secrets or OpenBao |
| `OpenChoreoReady` | Cozystack Postgres `portal`, its credentials as `openchoreo-control-plane/aep-portal-db` and a selectorless Service `aep-portal-db` whose EndpointSlice points at the management ClusterIP of `postgres-portal-rw`; Secret `backstage-secrets` (session secret generated once and kept, client secret from `spec.oidc.portalClient.secretRef`, unused Jenkins key); optional ConfigMap `aep-oidc-ca`; HelmRelease `aep-cp-openchoreo-control-plane` |
| `GatewayReady` | Finds the management mirror of `cluster-gateway-external`, takes `spec.clusterGateway.host` or `--gateway-host-template`, adds it to the gateway certificate, restarts `cluster-gateway` once per new certificate serial (annotation `aep.aenix.io/server-cert-serial`) and reports `status.gateway.url` |
| `IngressReady` | Ingresses `aep-cp-public` (console and API, TLS from `clusterIssuer` or `tlsSecretRef`) and `aep-cp-deny-direct-logs` in the tenant namespace, backed by the mirror of `gateway-default`; waits for the TLS Secret |
| `ContentReady` | The content below; does not block `Hardened` |
| `Hardened` | Prunes chart-managed broad bootstrap bindings as below, without needing an Organization |

`Ready` is True when these steps succeed. `AuditReady` is not implemented in
this release: it reports `NotImplemented` (`Disabled` without `spec.audit`) and
does not hold `Ready`. Status reports `outputs.kubeconfigSecretRef`,
`outputs.tenantNamespace`, `outputs.clusterName`, `endpoints` and `gateway`.

### Content

`ContentReady` delivers what developers build with, as `spec.content` selects:

| Content | Delivered as | Selected by |
|---|---|---|
| ClusterResourceTypes `cozystack-kafka`, `cozystack-postgres`, `cozystack-redis` (embedded from `resource-types/`) | Server-side applied to aep-cp | `resourceTypes`; empty applies all |
| `aep-component-types` 0.3.0 | HelmRelease `aep-cp-aep-component-types`, `buildOrganizations` = the build organizations below | `componentTypes`, default `true` |
| `aep-builds` 0.3.0, `plane=control` | HelmRelease `aep-cp-aep-builds`: `organizations[]` = `{namespace: <org>, workflowPlane: nonprod}`, `registry.host` = `builds.registryHost`, `openchoreo.apiUrl` = `https://api.<domain>`, `openchoreo.tokenUrl` = `oidc.tokenURL` or `<issuer>/protocol/openid-connect/token` | `builds` set |

Both charts come from `oci://ghcr.io/tym83/aep/charts` at fixed versions,
pinned by digest through the release inventory and mirrored like the other
charts; they depend on `aep-cp-openchoreo-control-plane`. Build organizations
are the Organizations on this control plane that have `spec.vcs` (the build
credential source, see [VCS](vcs.md#in-platform-builds)) and whose OpenChoreo
namespace exists on aep-cp; Organization changes update the lists. The nonprod
WorkflowPlane, its secret store, the registry push robot and the publisher
client stay prerequisites. `builds.publisherClientIDTemplate` (Go template with
`.Organization`) may render the chart default `aep-<org>-publisher` or one ID
shared by all build organizations: the chart takes a single override.

Content switched off is removed when the control plane owns it: Flux uninstalls
a removed chart, and a deselected resource type is deleted from aep-cp, which
breaks resources still using it. `ContentReady` is True when everything
selected is applied and both releases are Ready. Existing ClusterResourceTypes
without the control plane label are adopted only when `spec.adoption.confirm`
is set; objects labelled for an Organization or another ControlPlane are never
adopted. Content failures hold `Ready` but not `Hardened`, so they do not stop
Organizations.

The chart values never disable TLS verification and set the bootstrap
mappings to the system clients only (portal catalog reader, FinOps, RCA,
observer), so hardening normally finds nothing to prune. A privately issued IdP
certificate is trusted by the portal through `spec.oidc.caBundleSecretRef`
(`NODE_EXTRA_CA_CERTS`); the chart offers no CA option for the OpenChoreo API,
so its JWKS endpoint needs a publicly trusted certificate.

The chart's authorization bootstrap hook Job sets no `imagePullSecrets`, and
Helm recreates its ServiceAccount `openchoreo-authz-bootstrap` on every hook
run. With an authenticated mirror (`--image-registry-mirror` and
`--registry-secret`) the operator adds `aep-mirror-pull` to that
ServiceAccount and deletes, once, each hook pod admitted before the patch that
fails to pull; the Job recreates it with the secret.

Objects the operator creates are labelled `aep.aenix.io/control-plane=<UID>`,
never with the Organization label; existing objects without that label are
never adopted, so the step reports the conflict. `spec.suspend` stops all
changes and deletion; the last conditions stay as they were.

Deleting a ControlPlane requires the annotation
`aep.aenix.io/confirm-delete=<name>` and is blocked while an Organization
references it (or would use it as `--default-control-plane` without legacy
flags). The operator then removes, one layer at a time, the content charts,
the control plane chart, kgateway, chart sources, Ingresses and the network policy, the cluster,
the portal database, the tenant and the operator credential. The portal
database is kept, and with it the tenant, until the annotation
`aep.aenix.io/delete-portal-database=true` is set or the database is deleted by
hand.

### Organizations on a ControlPlane

An Organization resolves its aep-cp once per reconciliation, and never switches
implicitly:

1. `spec.controlPlaneRef`, recorded in `status.controlPlaneRef` when accepted;
2. otherwise `--default-control-plane`, but only when no legacy kubeconfig
   (`--aepcp-kubeconfig-secret`, `--aepcp-kubeconfig`) is configured: with
   both, unreferenced Organizations stay on the legacy aep-cp;
3. otherwise the legacy `--aepcp-*` flags, exactly as before ControlPlanes.

On a ControlPlane the Organization connects with the scoped credential in
`status.outputs.kubeconfigSecretRef` (refused unless the Secret carries the
ControlPlane's label), finds the gateway mirror in the ControlPlane tenant
namespace, admits its tier agents to it and dials `status.gateway.url`; the
gateway CA, OpenChoreo objects, fleet data planes and deletion use the same
credential. Its tenants are created right away, but `ClustersReady` waits with
reason `ControlPlaneNotReady` (phase `Pending`) until the ControlPlane reports
`OpenChoreoReady`, `GatewayReady` and `Hardened` for its generation and has
published its credential and gateway. A missing default ControlPlane is
reported the same way. ControlPlane status changes requeue the Organizations it
serves.

An Organization is `Rejected` when `spec.controlPlaneRef` names a ControlPlane
that does not exist, or when a ControlPlane tenant namespace equals one of the
Organization's (`tenant-<org>`, `tenant-<org>-<tier>`); a ControlPlane is
`Rejected` for the same collision, whichever came first. The reference is
immutable once set. On an Organization that was already reconciled without one,
the operator accepts a new reference only with the annotation
`aep.aenix.io/adopt-control-plane=<name>` naming the same ControlPlane;
otherwise the Organization is `Rejected`.

### Gateway host in air gaps

Without `spec.clusterGateway.host` the gateway host comes from
`--gateway-host-template`, by default `nip.io`, which needs public DNS. Set a
fixed host such as `cluster-gateway.<name>.aep.internal` for air-gapped
installations: it goes into the gateway certificate, and every tier cluster of
the Organizations on this control plane resolves it to the management mirror
ClusterIP of the gateway. The operator adds that entry through the Kubernetes
application's `addons.coredns.valuesOverride`: a separate CoreDNS server block
for the host in the coredns chart's `extraConfig` (Cozystack v0.36 or later),
next to the unchanged Cozystack Corefile. External fleet clusters do not get
this entry; they need a DNS record for the host in corporate DNS.

## Kafka

Cozystack's Kafka application has no client authentication, so the bridge does
not use it. Allowing `Kafka` in `services.allowedKinds` makes the bridge render
Strimzi resources inside the tier application cluster, in `aep-kafka`, where the
operator-delivered Strimzi 0.50.0 watches only its own namespace:

- `Kafka <name>` (KRaft, node pools) with a single internal listener on 9093
  that requires TLS and SCRAM-SHA-512 and whose NetworkPolicy admits only the
  claim namespace; `simple` authorization. Internal topic replication factors
  are `min(replicas, 3)`, `min.insync.replicas` is 1 below three brokers, else 2.
- `KafkaNodePool <name>-pool` with combined controller/broker nodes, a
  persistent claim of `size` (`deleteClaim` only when the claim is not retained)
  and requests/limits from `resourcesPreset`: one CPU unit is 250m and one memory
  unit 128Mi on the tier ceiling scale, at least 1Gi.
- One `KafkaTopic` per requested topic (object `<name>-<topic>`, topic name
  `<topic>`), and `KafkaUser <name>-app` allowed Read, Write, Describe and
  DescribeConfigs on exactly those topics plus Read and Describe on consumer
  groups prefixed `<name>-`. No cluster permissions are granted.

`<name>` is the claim's application name (at most 26 characters, shortened with
a hash suffix if a longer one ever appears) so broker pod names stay DNS labels.
The bridge server-side applies these objects with the `aep.aenix.io/managed-by`
label and claim UID annotation, refuses foreign objects, and is limited by a
Role and an admission policy to bridge-managed Strimzi objects in `aep-kafka`.
`Ready` requires the Kafka, every topic and the user to report Ready for their
current generation and the node pool to have all requested nodes. The
connection Secret `<claim>-conn` holds `bootstrap.servers`, `host`, `port`,
`username`, `password`, `sasl.jaas.config`, `security.protocol` (`SASL_SSL`),
`sasl.mechanism` (`SCRAM-SHA-512`), `topics`, `ca.crt` and `uri`; clients connect
to `<name>-kafka-bootstrap.aep-kafka.svc.cluster.local:9093` directly. Topics
removed from a claim are deleted unless the claim is retained, in which case they
are marked `aep.aenix.io/orphaned=true` like the cluster on claim deletion.
Kafka has no Cozystack backup class: a tier backup policy is reported as
`BackupReady=False/NotSupported`. Strimzi operands use the restricted Pod Security
provider; with an authenticated mirror, Strimzi passes `aep-mirror-pull` to broker
and entity operator pods. External fleet clusters do not receive Strimzi.

## Deletion and recovery

Deletion requires `aep.aenix.io/confirm-delete=<organization-name>`. Before starting,
list Components on aep-cp and ServiceClaims in every existing tier; any object or
failed read blocks deletion and reports its identity/reason. Retained Postgres or
Redis applications, and Kafka clusters left in a tier's `aep-kafka` namespace,
also block deletion to prevent loss through tenant removal.
Persist the successful preflight, then delete owned OpenChoreo resources, remote
HelmReleases/sources, CCNPs, bridges/RBAC, workers/clusters, child tenants, parent,
organization namespace, admission rules and management credentials, waiting for
each dependency layer. UID preconditions protect against replacement races.
The organization admission rules do not apply to updates of an object that is
already being deleted, so garbage collection and OpenChoreo cleanup can release
their finalizers. Only the operator can start deletion of reserved objects. Deletion re-applies these rules
first, which also fixes organizations created by older releases.

Stop user writes before confirming deletion. Stage 1 does not provide an atomic
cross-cluster write freeze; the deletion preflight is not a transaction. Keycloak
groups remain for audit/federation cleanup by an administrator.

If an operator token expires during a long outage, remove only the matching owned
`operator-access-<org>-<tier>` Secret to explicitly request super-admin bootstrap
again. Never delete an agent CA Secret as a routine retry: that changes trust.
Back up these CA Secrets and monitor CA expiry; reconciliation stops within 30 days
of expiration to require a planned trust rotation.

## Still manual / follow-up

- DNS records for `console.<domain>` and `api.<domain>`, the IdP realm,
  federation and group mapping, the portal and CLI clients and per-organization
  service clients (publisher, change), and review of unsafe bindings without
  chart management labels. A legacy aep-cp additionally needs its installation,
  SAN configuration/restart and OIDC by hand.
- ESO and the `default` ClusterSecretStore/backend on every data plane. The core
  charts do not provision a secret backend; components needing SecretReference
  require that prerequisite.
- IdP federation mapping, tenant OIDC/user RBAC, CA rollover/CSR signing,
  decommission workflow, and environment-removal migration are follow-ups. Removed
  environments are not pruned automatically; removal requires platform review.
- Live end-to-end installation and isolation testing remain required. Unit tests
  and Helm rendering do not validate the stand's runtime APIs or CEL admission.

See [known gaps](known-gaps.md) for upstream and credential dependencies.

## External fleet

The optional fleet reconciler runs in the same operator with its own queue.
See [fleet](fleet.md) for approved assignments, OCM hub isolation, outbound-only
ManifestWork delivery, bootstrap and readiness gates. Tenant clusters retain the
existing Flux path. See [VCS](vcs.md) for the optional installation-wide GitLab
profile; the operator never deploys GitLab per organization.
