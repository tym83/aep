# Organization operator (stage 1)

The operator runs on the Cozystack management cluster. It provisions dedicated
application clusters and registers them with an **existing** OpenChoreo 1.4.0-m.3
control plane. It does not install aep-cp or change its Helm release.

## Installation

Build the operator image with `docker build -f Dockerfile.operator -t <image> .`
and the bridge image with the repository's default Dockerfile. Publish images to
your own registry before deployment; the chart's image names are placeholders.

```sh
helm upgrade --install aep-operator charts/aep-operator \
  --namespace aep-system --create-namespace \
  --set image=<operator-image> --set bridgeImage=<bridge-image> \
  --set aepcpKubeconfigSecret=tenant-aep-cp/kubernetes-aepcp-admin-kubeconfig \
  --set aepcpTenantNamespace=tenant-aep-cp --set aepcpCluster=aepcp
```

Cozystack's aggregated Tenant/Kubernetes/KubernetesNodes APIs, Kamaji, Flux OCI
sources and Helm controllers, and Cilium CCNP must already be installed. The
operator namespace must exist. Supply aep-cp credentials with permission to manage
organization namespaces, OpenChoreo objects and admission policies, read gateway
Deployments/Secrets, and create SelfSubjectReviews. Its Kubernetes identity must
be dedicated to the operator, distinct from OpenChoreo's API identity and users.

The operator checks installation-wide `ClusterAuthzRoleBindings` before tenant
provisioning on every Organization reconciliation, including startup reconciliation.
`ControlPlaneHardened` reports False with the sorted names of remaining bindings
for any `groups` claim, `client_id=service_mcp_client`, or the shared
`workload-publisher-binding`. Failed inspection also reports False. Ready requires
this condition to be True. Invalid Organizations still poll every minute; pending
ones retry with their normal provisioning cadence/backoff. At least one Organization
must exist for this reconciliation-based installation check to run.

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
      services: {allowedKinds: [Postgres, Redis], maxPreset: u1.large}
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
   Its ClusterRole only lists Nodes and ServiceClaims; namespace Roles handle
   data-plane Secrets/ConfigMaps, listing kube-system bootstrap tokens and issuing
   tokens for the two named SAs. Credentials remain on management in owned Secrets.
5. Generate one ECDSA P-256 CA per tier, valid three years. Store it in
   `aep-system/agent-ca-<org>-<tier>` and copy cert+key to
   `openchoreo-data-plane/aep-agent-ca`. Copy the aep-cp gateway CA **certificate**
   into `cluster-gateway-ca` ConfigMap. Install OCI sources and remote HelmReleases
   for kgateway-crds/ kgateway v2.3.11 and openchoreo-data-plane 1.4.0-m.3, with
   explicit dependencies, Cozystack sharding/target labels and the requested
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
| `--aepcp-kubeconfig-secret` | Required unless local file; management `namespace/name`, key `super-admin.svc` |
| `--aepcp-kubeconfig` | Local file; mutually exclusive with Secret reference |
| `--aepcp-tenant-namespace` | `tenant-aep-cp` |
| `--aepcp-cluster` | `aepcp` |
| `--gateway-host-template` | `cluster-gateway.{{.DashedIP}}.nip.io`; also exposes `.ClusterIP` |
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

## Deletion and recovery

Deletion requires `aep.aenix.io/confirm-delete=<organization-name>`. Before starting,
list Components on aep-cp and ServiceClaims in every existing tier; any object or
failed read blocks deletion and reports its identity/reason. Retained Postgres or
Redis applications also block deletion to prevent loss through tenant removal.
Persist the successful preflight, then delete owned OpenChoreo resources, remote
HelmReleases/sources, CCNPs, bridges/RBAC, workers/clusters, child tenants, parent,
organization namespace, admission rules and management credentials, waiting for
each dependency layer. UID preconditions protect against replacement races.

Stop user writes before confirming deletion. Stage 1 does not provide an atomic
cross-cluster write freeze; the deletion preflight is not a transaction. Keycloak
groups remain for audit/federation cleanup by an administrator.

If an operator token expires during a long outage, remove only the matching owned
`operator-access-<org>-<tier>` Secret to explicitly request super-admin bootstrap
again. Never delete an agent CA Secret as a routine retry: that changes trust.
Back up these CA Secrets and monitor CA expiry; reconciliation stops within 30 days
of expiration to require a planned trust rotation.

## Still manual / follow-up

- aep-cp installation, SAN configuration/restart, OIDC and review of unsafe bindings without chart management labels.
- ESO and the `default` ClusterSecretStore/backend on every data plane. The core
  charts do not provision a secret backend; components needing SecretReference
  require that prerequisite.
- `backupPolicy` is represented in the API but Backups Plan materialization is not
  implemented by the current bridge. Do not interpret it as a backup guarantee.
- Kafka is not supported by the existing bridge and is rejected explicitly.
- IdP federation mapping, tenant OIDC/user RBAC, CA rollover/CSR signing,
  decommission workflow, and environment-removal migration are follow-ups. Removed
  environments are not pruned automatically; removal requires platform review.
- Live end-to-end installation and isolation testing remain required. Unit tests
  and Helm rendering do not validate the stand's runtime APIs or CEL admission.

See [known gaps](known-gaps.md) for upstream and credential dependencies.
