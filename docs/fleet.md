# External cluster fleet

External clusters are application clusters, not additional AEP management sites.
`ExternalCluster` is namespaced alongside its organization's ChangeRequests
(default `tenant-{org}`). Its immutable spec records organization, logical tier,
environment, trust (`untrusted`, `standard`, `regulated`), provider/distro and the
approved change. Discovery alone never grants an assignment.

## Approval and admission boundary

Submit through the existing authenticated change API:

```yaml
organization: acme
operation: cluster-attach
model: normal
target:
  kind: Cluster
  name: eks-east
  environment: production
desired:
  tier: prod
  trustLevel: regulated
  provider: aws
  distro: eks
justification: Dedicated production account with audit forwarding to SIEM
```

The environment must already exist in `Organization.spec.environments` with the
same tier. A platform-team human approves every attachment. Prod or regulated
attachments additionally require a **different** member of `aep-security`.
The requester, service accounts, system approvals and emergency/standard models
cannot bypass this requirement. Untrusted prod is rejected outright. Security
must verify cloud API and Kubernetes audit forwarding, audit reconciliation,
a separate cloud account/subscription per tier, and no human cluster-admin IAM.
These compensating controls are approval evidence, not automatically discovered.

The change controller creates the assignment idempotently. A different change
cannot silently replace it. The registrar and bootstrap CLI independently read
the applied change, compare its complete desired assignment and evaluate SoD.
Only the change controller receives create access to ExternalCluster. Do not
bind tenant users to its write/status permissions or allow them to mutate
ChangeRequest status outside the authenticated change API. As with the existing
change service, cluster administrators are inside the Tier-0 trust boundary.

The organization reconciler reserves assigned environments; the fleet reconciler
is their sole writer. It binds the Environment to the new DataPlane only after
ManifestWork policies are Applied/Available at the current generation and the
OpenChoreo gateway reports the agent connected. Duplicate environment assignments
fail closed. Existing workloads are not migrated automatically.

## Hub installation profile (not deployed by AEP)

Use the official [OCM Helm charts](https://open-cluster-management.io/docs/getting-started/installation/).
Pin both `cluster-manager` and `klusterlet` to **1.4.0**. For example, as a separate
platform installation step:

```sh
helm upgrade --install cluster-manager cluster-manager \
  --repo https://open-cluster-management.io/helm-charts --version 1.4.0 \
  --namespace open-cluster-management --create-namespace \
  --set createBootstrapToken=false --set createBootstrapSA=false
```

Do not enable global auto-accept/auto-approve users. The AEP registrar approves
only a CSR from the dedicated bootstrap identity whose cluster label, X.509
subject/group and usages match the approved assignment. OCM handles subsequent
agent certificate renewals. No hub chart is vendored here.

The task's management-as-hub topology is supported when `fleet.hubConfigSecret`
is empty. DESIGN 5.14 instead recommends an isolated Tier-0 tenant cluster
`aep-fleet`, separate from management and `aep-cp`; use that topology for production.
Set a Secret containing key `config` with a restricted hub client, and grant on
the hub the OCM/inventory/CSR permissions in the operator chart. Management keeps
Organizations, ChangeRequests, ExternalClusters and private agent CAs. Hub users
are platform operators only. Encrypt and audit hub storage: ManifestWork contains
agent **leaf** keys and grants administrative powers over spokes. Back up hub etcd
and management CA Secrets with the same recovery discipline as `aep-cp`.

Install the upstream cluster-inventory-api **v0.1.0** CRD serving
`multicluster.x-k8s.io/v1alpha1` on the hub. AEP produces ClusterProfiles in
`aep-{org}` and stores their references in ExternalCluster. Do not also enable an
OCM inventory writer for these objects. This intentionally follows the requested
v1alpha1 contract; DESIGN 5.14 now specifies v0.2.0/v1alpha2. That migration requires
an explicit compatibility decision before upgrading the inventory CRD.

## Delivery and network

Enable `fleet.enabled`, set `fleet.gatewayURL` to a routable `wss://.../ws`
endpoint with a valid gateway certificate, and populate `fleet.chartsPVC` with:

```sh
sh charts/aep-operator/fleet-stage.sh /path/to/staged-charts
```

The operator image contains Helm. It templates local pinned artifacts without
cluster credentials; no spoke kubeconfig is stored or used. Mirror these artifacts
and all their pinned images for air-gap installations. The stage script packages
local AEP charts and downloads upstream packages, but installs nothing.
The OpenChoreo/kgateway/Kyverno/policy versions are shared with tenant delivery
in `internal/operator/delivery.go` and `policies.go`. Upgrade staged packages and
operator together; canary a nonprod attachment before production rollout.

The registrar creates an organization ManagedClusterSet, its namespace binding,
a label-constrained Placement, accepted ManagedCluster and ClusterProfile. It
sends raw rendered resources through OCM ManifestWork: OpenChoreo data plane,
kgateway, ServiceClaim/bridge-appcluster contract, Kyverno, baseline and tier
policies. Standard trust uses the organization baseline mode; untrusted and
regulated force baseline enforcement. Prod additionally requires a cosign key.
Kyverno readiness precedes policy delivery. Works are split below API object-size
limits; CRD documentation strings are omitted without removing validation. The
pinned OpenChoreo chart emits agent Certificate/Issuer resources even with
`generateCerts=false`; the renderer excludes those resources so cert-manager cannot
overwrite AEP-issued leaf certificates. Certificate changes roll the agent pod. Restricted PSS namespaces and pod
security contexts apply to AEP-delivered components. Install cert-manager and
Gateway API CRDs on the target first; they are prerequisites, not duplicated by
AEP. The target must support the admission APIs in the pinned bundles (Kubernetes
1.35 is the rendering baseline).

Each external cluster has a unique plane ID and CA. The CA key remains on
management. Only a 90-day client certificate/key reaches the spoke; renew at 30
days remaining. Alert on reconcile errors and certificate expiry. Monitor the
four ExternalCluster conditions: `Registered`, `Joined`, `DataPlaneConnected`,
`PoliciesReady`; creation of a ManifestWork is never treated as policy readiness.

Allow spoke egress to hub Kubernetes API (normally TCP 6443/443), OpenChoreo
WebSocket gateway (configured TLS port), DNS and image registries. There is no
management/hub ingress path into the spoke. Application ingress is a separate
customer network concern. OCM cluster-proxy can provide an outbound reverse
administration tunnel as an optional separately operated add-on; this delivery
path needs no proxy. Hub failure prevents changes, not existing workloads.

Cozystack tenant networks do not extend to these clusters. ServiceClaim CRDs and
RBAC/admission contract are delivered, but **no management bridge is pointed at
a spoke kubeconfig**. Managed Postgres/Redis require a separately approved TLS
endpoint/backend or cloud ResourceType. Installing the contract alone does not
provision a reachable Cozystack service; in-cluster bridge execution and external
service credential transport remain a separate integration.

## Join

After the change becomes Applied and the registrar creates the hub namespace:

```sh
aep-fleet join --management-config ./management.conf --hub-config ./hub.conf \
  --namespace tenant-acme --cluster eks-east > join-values.json
# On a workstation with access to the target (not from management):
helm upgrade --install klusterlet klusterlet \
  --repo https://open-cluster-management.io/helm-charts --version 1.4.0 \
  --namespace open-cluster-management --create-namespace -f join-values.json
```

The CLI creates a dedicated bootstrap account and requests a ten-minute token
only after independently verifying the applied change. It rejects a server-issued
lifetime exceeding 15 minutes. Output is sensitive; use `umask 077`, deliver it
securely and delete it after joining. Alternatively supply `--target-config`
and `--execute`; Helm receives values on stdin, never a token argument. Run
`aep-fleet join -h` for flags. The bootstrap identity can submit CSRs but cannot
approve them or read other spoke credentials. Re-bootstrap after Joined is deliberately refused; use the platform recovery procedure.
Delete its ServiceAccount and
bootstrap RBAC after a successful join; expired tokens cannot bootstrap again.

## Detach and verification

DESIGN does not define a `cluster-detach` operation; none is added implicitly.
ManifestWork uses orphan deletion to avoid deleting workloads during an accidental
hub object loss. De-registration is a platform maintenance procedure: stop new
placements, drain/migrate workloads through approved changes, unbind environments,
revoke the gateway CA registration, remove OCM work/registration and bootstrap
RBAC, uninstall spoke agents and finally remove inventory/assignment. Do not delete
an assignment to migrate an active environment. Automated detach is deferred.

Local checks cover approval bypasses, idempotency, assignment tampering, CSR
identity, leaf trust/lifetime, stale readiness and pinned Helm rendering:

```sh
AEP_FLEET_CHART_TEST_DIR=/path/to/staged-charts go test ./internal/operator
```

Live acceptance still requires a disposable spoke behind a firewall allowing
only the documented egress: submit the change, prove one approver cannot activate
prod, join, observe all four conditions, deploy a workload, verify policy denials,
interrupt hub access and verify existing service traffic continues. Repeat on the
supported provider matrix (Cozystack/other distribution/EKS/AKS/GKE). No live
cluster test or deployment is performed by this repository change.

## Environment ownership

Declare external placement when creating the environment:

```yaml
spec:
  environments:
    - name: edge-dev
      tier: nonprod
      changeModel: required
      dataPlane: {external: edge-west}
```

The Organization controller never creates this Environment on a Cozystack
DataPlane, even before attachment or while ExternalCluster approval is invalid.
OpenChoreo `dataPlaneRef` is immutable: migration requires a new Environment.
Admission forbids removing an external declaration or changing its tier or
assignment. Retain the declaration during detach as an audit record.

## Suspension and detach

`spec.suspended: true` revokes hub acceptance and removes the external DataPlane,
so its client CA no longer authorizes connections. Invalid assignment approvals or
Organization mismatches use the same quarantine path. Registered records the
accepted registration; Ready aggregates current join, connection and policy health.
An external outage never fails the Organization's environment reconciliation.

Request `cluster-detach` with model `normal`, the Cluster target and environment,
and `desired: {tier: prod, uid: <ExternalCluster UID>}`. Platform approval is always
required, with a distinct security approver for prod. The UID prevents a stale
request from detaching a recreated assignment. Drain workloads before requesting it.
The change waits for finalization before Applied. Organization deletion also waits
for every ExternalCluster finalizer before deleting the control-plane namespace.

Cleanup revokes hub acceptance, removes ManifestWorks with **Orphan**, waits for
work finalizers, removes the DataPlane and management CA/leaf Secrets, deletes
bootstrap credentials and ClusterProfile, then removes ManagedCluster. Orphan is
intentional: customer workloads and agent resources remain for site-admin removal;
detach does not promise remote erasure on an offline cluster. Environment records
remain with a now-missing immutable DataPlane reference. Each new assignment UID
gets a distinct plane identity, CA and leaf; detached credentials cannot be reused.
The management cluster is the default hub; a separate hub remains optional.
ClusterProfile discovery prefers v1alpha2 when served and falls back to v1alpha1
(OCM 1.4.0). Its API version is not part of immutable assignment approval.
