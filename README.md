# Aenix Enterprise Platform

AEP connects OpenChoreo application resources to Cozystack infrastructure.
Licensed under Apache-2.0.

## Bridge

The bridge runs on the **management cluster**, one instance per organization and
tier, and watches ServiceClaims in one application cluster. The application
cluster never receives management credentials. Supported backends are Postgres
and Redis, accessed through the dynamic client without Cozystack Go types.

### Install

1. Install `charts/aep-bridge-appcluster` on the application cluster. This installs
   the ServiceClaim CRD, the `aep-system/aep-bridge` ServiceAccount, RBAC, and
   admission policies. OpenChoreo's data-plane agent gets explicit claim RBAC.
2. Supply that ServiceAccount's restricted kubeconfig and rotating token to an
   existing Secret named `aep-bridge-app-credentials` in the management tenant.
   Keys are `kubeconfig` and `token`. Configure kubeconfig `tokenFile` as
   `/app-credentials/token`; keep CA verification enabled and use the reachable
   application API address. A platform credential rotator must update the token
   before expiration. Secret volumes are mounted without `subPath` so rotation
   reaches client-go's `BearerTokenFile` transport.
3. Install the management chart **in the tier tenant namespace**:

   ```sh
   helm upgrade --install aep-bridge ./charts/aep-bridge \
     --namespace tenant-aepdev-nonprod \
     --set organization=aepdev --set tier=nonprod --set clusterID=apps-unique-id \
     --set tierDeletionPolicy=Delete
   ```

4. Apply `resource-types/cozystack-postgres.yaml` and
   `resource-types/cozystack-redis.yaml` to the OpenChoreo control plane.

Kubernetes 1.30+ is required for stable ValidatingAdmissionPolicy. The supplied
OpenChoreo templates target its 1.4 ResourceType CEL contract. Install only one
application-side chart per application cluster. CRDs under a Helm chart's
`crds/` directory are installed on first install; apply the generated CRD
explicitly before upgrades. `config/app-cluster` and `config/mgmt` also contain
standalone example manifests with explicit example namespaces.

Build and run locally with Go 1.26:

```sh
make build test lint
go run ./cmd/bridge \
  --mgmt-kubeconfig=/path/to/management-kubeconfig \
  --app-kubeconfig=/path/to/application-kubeconfig \
  --app-token-file=/path/to/rotating-token \
  --organization=aepdev --tier=nonprod \
  --tenant-namespace=tenant-aepdev-nonprod --cluster-id=apps-unique-id \
  --allowed-kinds=Postgres,Redis --tier-deletion-policy=Delete \
  --config=config/mgmt/policy.yaml
```

Management authentication defaults to in-cluster credentials. Leader election
uses a lease on the management cluster in the configured tenant, with an ID
hashed from cluster, organization, and tier. Changing `cluster-id` changes
application identities: use a stable, unique cluster incarnation ID. Metrics
are disabled; health probes listen on port 8081. The controller polls each claim
every 15 seconds to observe management readiness, rotated credentials, and
endpoint changes. Only ServiceClaims are cached; ownership and credential reads
use direct clients. There is no periodic orphan collection or list-driven deletion.

### Claim contract and limits

```yaml
apiVersion: aep.aenix.io/v1alpha1
kind: ServiceClaim
metadata:
  name: orders-db
  namespace: dp-payments-dev
spec:
  kind: Postgres
  resource: {project: payments, name: orders-db, environment: dev}
  parameters:
    version: v17
    users: {app: {}}
    databases:
      appdb:
        roles: {admin: [app]}
  environmentParameters:
    resourcesPreset: t1.small
    size: 2Gi
    replicas: 1
  deletionPolicy: Delete
```

Only the OpenChoreo agent can create or change claims through the supplied
admission policy. The bridge has an exception for updates with unchanged spec
so it can maintain finalizers; status uses its own subresource. Bridge output
writes require the `dp-` namespace prefix and the management label on both new
and existing objects. RBAC cannot scope reads by label: the app-cluster Role
allows named GETs of Secrets, Services and EndpointSlices, but no list/watch.
The controller additionally checks a claim UID annotation before overwriting outputs.

The config allowlist can narrow, but never expand, the built-in safe parameter
set. Missing sizing fields are rejected to avoid bypassing limits with chart
defaults. Preset ceilings compare **both CPU and memory**, not preset names.
Defaults are `u1.large`, `100Gi`, and 3 replicas. Legacy flat preset aliases are
not accepted. PostgreSQL users must be empty objects: passwords, replication,
createRole and bypassRls are rejected, even if false. Databases accept only
`roles.admin` and `roles.readonly` referencing declared users; extensions and
other nested parameters are rejected. Version, users and databases are immutable
once provisioned. Kind and resource identity are immutable in the CRD.

For multiple PostgreSQL databases the connection selects the first database in
lexical order, then its first admin (or readonly) user, otherwise the first
declared user. Declare database role membership for a usable connection.
Redis exports database `0` and user `default`.

### Reconciliation and lifecycle

Application names are `aep` + 10 lowercase base32 SHA-256 characters from
`clusterID/claimNamespace/claimName` + `-` + up to 12 alphanumeric resource-name
characters. The maximum is 26 characters. A different claim UID at that name is
always `NameConflict`; recreated claims are never silently adopted.

`Ready=True` requires Cozystack Ready and WorkloadsReady, a current and Ready
Flux HelmRelease, matching release values, and the accepted application spec
hash. Cozystack applies schema defaults: `applied-spec-hash` hashes the accepted
full spec, while `claim-spec-hash` tracks the request. The bridge never relies
on Application metadata.generation (absent in Cozystack). Changes first publish
`Ready=False/Updating`; reasons, messages and upstream transition timestamps
are carried into Provisioned, Ready and Degraded conditions. Connection failures
also prevent Ready. No password or URI is written to status.

The bridge reads credentials exclusively through TenantSecret, then applies
`<claim>-conn`, a selectorless `<claim>` Service, and an EndpointSlice using
field manager `aep-bridge`. The slice targets the backend management Service's
ClusterIP, reachable from pods in the same Cozystack tenant (verified in S1).
Endpoint drift is corrected and reported as `Degraded=True/EndpointDrift` for
that reconcile. The next successful reconcile clears it. Credential rotation is
propagated on every reconcile. Output resources have claim owner references.

Postgres uses `<release>-credentials`, `<release>.tenant-ca`, and
`<release>-rw:5432`; Redis uses `<release>-auth` and `rfrm-<release>:6379`.
The local host is `<claim>.<namespace>.svc.cluster.local`. PostgreSQL's generated
URI requests TLS (`sslmode=require`); consumers needing CA verification must
mount `ca.crt` and configure their driver. Backend certificates do not contain
the local alias, so hostname verification needs the backend server name or a
platform certificate change. Redis's upstream default is plaintext; v0 cannot
change TLS through claims, and its `ca.crt` output is empty.

`--tier-deletion-policy=Retain` is an operator-level retain override. Nonprod
with tier policy Delete honors the claim policy (default Delete). **Prod always
retains**, even when both policies say Delete. Retained applications get
`aep.aenix.io/orphaned=true` before finalizer removal. Delete uses UID and
resourceVersion preconditions and waits for disappearance. Annotation
`aep.aenix.io/paused=true` blocks application writes and finalization.

### Verification and remaining integration work

```sh
make generate        # pinned controller-gen v0.21.0
make build test lint
COZYSTACK_REPO=/path/to/cozystack go test ./internal/kinds -v
# Inspect a Git ref without changing that checkout:
COZYSTACK_REPO=/path/to/cozystack COZYSTACK_REF=upstream/main go test ./internal/kinds -v
helm lint charts/aep-bridge --set organization=test --set clusterID=test
helm lint charts/aep-bridge-appcluster
make docker-build    # ghcr.io/tym83/aep-bridge
```

Unit and fake-client tests cover naming, nested allowlists, limits, readiness,
API defaulting, conflicts, pause, retention, credential rotation and endpoint
drift. Fake clients do not implement Kubernetes SSA/admission; tests intercept
SSA to verify its field manager and resulting objects. Live admission, SSA
conflict behavior and an end-to-end OpenChoreo deployment remain integration
checks. No live cluster is modified by these tests.

Verified against local Cozystack `upstream/main` at
`054b4ef4bc391b78e1ef0e04b813b2bfd83855f6`: application name validation
is DNS-1035 plus `53 - len(release prefix)` (`pkg/registry/apps/application/rest.go`),
giving 44 for Postgres and 47 for Redis, subject to a tighter ApplicationDefinition
name schema. Redis version and naming patterns are present in chart templates;
the master Service itself is created by its operator. Re-run chart contract tests
against the deployed Cozystack revision before release.

The optional prod deletion VAP is disabled in Helm by default and remains a
**draft**: applicability to aggregated APIs must be verified in spike S12.
The controller's unconditional production retention does not depend on it.
Optional `status.facts` (engine version and last backup) is reserved in the API
but is not populated in v0: chart version must not be misreported as engine
version. The image build and live-cluster integration are separate release gates.

### Layout

- `api/v1alpha1`: ServiceClaim API and generated deepcopy.
- `cmd/bridge`: two-cluster wiring, token file and leader election.
- `internal/bridge`: reconciliation, readiness, connections and finalization.
- `internal/kinds`: centralized backend name/port/credential mapping.
- `internal/policy`: safe parameters, limits, naming and retention.
- `config`: generated CRD and standalone RBAC/admission examples.
- `charts`: management and application cluster Helm charts.
- `resource-types`: OpenChoreo PostgreSQL and Redis templates.
