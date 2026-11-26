# Portal API contracts

The Backstage AEP backend exposes `/api/aep/v1/*`. It requires a Backstage user
credential and `x-openchoreo-token` (the OpenChoreo OIDC access token). It sends
only that user access token upstream as `Authorization: Bearer ...`. It never
uses catalog service credentials for AEP requests, decodes roles without
verification, or writes to Kubernetes or Cozystack directly.

Both upstream services MUST verify signature, issuer, audience and expiry, and
authorize organization membership on every request. The read API must derive
membership and roles from authenticated identity; request paths are not proof
of membership. Decision authorization, separation of duties, protected
environments and operation validation remain enforced by the change API and
OpenChoreo. UI role checks are only presentation.

## Read API

`aep.readApiUrl` is an origin/base path without `/v1`. `cmd/read-api` implements these
endpoints using management-cluster state. It verifies OIDC issuer, signature,
expiry and audience; `--issuer` and `--audience` are required. The portal also
includes a development mock.

### GET /v1/me

```json
{"username":"alice","organizations":[{"name":"acme","roles":["developer","approver"]}]}
```

Return only existing cluster-scoped `aep.aenix.io/v1alpha1` Organizations,
using their DNS-label names. Membership and roles come exclusively from verified
token groups `aep-<org>-<role>` or `/aep-<org>-<role>`, with roles `developer`,
`platform`, `approver`, `admin`, `auditor`, and `finance`. Organizations and roles are sorted
and deduplicated. Unknown roles and missing Organizations are not returned.
Organization routes return 403 for non-members, or 404 for members of an
Organization that no longer exists. The portal
selects the first organization by default and offers a selector for others.
Role `approver` enables the Changes page for that organization.

Finance-only membership permits `/v1/me` and organization costs (200), but
returns 403 on services, changes and compliance. Membership in additional roles
is additive and retains their permissions. Finance has no OpenChoreo role.

### GET /v1/organizations/{org}/services

Return only applications created by the AEP bridge for this organization,
including tenant-environment clusters and managed services. `Postgres` and
`Redis` must have `aep.aenix.io/managed-by=bridge`; `Kubernetes` must be named
`apps` and carry the operator ownership label `aep.aenix.io/organization` equal
to the Organization UID. Only namespaces `tenant-<org>-nonprod` and
`tenant-<org>-prod` are read. Do not enumerate arbitrary Cozystack applications
or return credentials. Row names are `<namespace>/<Kind>/<application-name>`
to avoid collisions across tiers and kinds. Only the AEP organization, tier,
project, resource, environment and managed-by labels are exposed. The bridge
already copies project/resource/environment identifiers into these labels.

```json
{
  "items": [{
    "name": "tenant-acme-prod/Postgres/postgres-orders",
    "kind": "service",
    "labels": {
      "aep.aenix.io/project": "payments",
      "aep.aenix.io/resource": "orders-db",
      "aep.aenix.io/environment": "prod"
    },
    "application": {
      "name": "postgres-orders",
      "namespace": "tenant-acme-prod",
      "consoleUrl": "https://console.example.org/tenant-acme-prod/postgres-orders"
    },
    "claim": {
      "name": "orders-db",
      "generation": 2,
      "conditions": [{
        "type": "Ready", "status": "False",
        "reason": "Updating",
        "message": "Waiting for the applied spec and current HelmRelease generation"
      }]
    }
  }]
}
```

`kind` is `cluster` or `service`; `name` uniquely identifies a row. Labels carry
human-readable display names and resource/project/environment identifiers.
Resource cards match `openchoreo.io/resource` and `openchoreo.io/project`
annotations against these labels; aliases must be normalized by the read service
if display names differ from identifiers. `application.consoleUrl` must be an
HTTP(S) console link. It is omitted unless `--console-url-template` is set;
this Go template receives `Namespace`, `Kind` (the Cozystack kind), and `Name`.
Rendered links must be HTTP(S) without embedded credentials.

In v0, application clusters and ServiceClaims are not read directly.
`claim.name` is the name portion of the app annotation
`aep.aenix.io/claim-ref` (`<namespace>/<name>`); it is empty when absent,
including for operator clusters. `claim.generation` is the **application**
generation, not an asserted ServiceClaim generation. Conditions are computed
with `bridge.Readiness(app, nil, false)`: app `Ready` becomes `Provisioned`;
`WorkloadsReady` is the source of `Ready`, but without HelmRelease and spec-match
evidence it is conservatively overridden to `Ready=False`, reason `Updating`.
The first false app `Ready` or `WorkloadsReady` condition becomes
`Degraded=True`; otherwise `Degraded=False`. Missing source conditions are
Unknown/Pending. Source `observedGeneration` values remain unchanged so stale
observations cannot become current. No synthetic claim generation or successful
HelmRelease observation is invented. Consequently a healthy app remains
Updating in v0; confirming Ready requires a future extension with more evidence.

For the portal, a current `Degraded=True` wins over `Ready=True`; a current
`Ready=True` is Ready; everything else (including stale or absent conditions)
is Updating. The message/reason is displayed without HTML interpretation.

### GET /v1/organizations/{org}/changes

Requires organization `approver` or `auditor`; `admin` alone does not grant
this read. The current portal only enables its Changes page for `approver`.
Returns `{"items": [ChangeRequest, ...]}`,
using the existing `aep.aenix.io/v1alpha1` object shape. Include metadata `name`
and `resourceVersion`, spec `organization`, `requester`, `operation`, `target`,
`justification`, optional `window`, and status `phase` and `approvals`.
Requests are read from `tenant-<org>`, matching the change service default;
`--org-namespace-template` must match the change service if overridden. Results
also require `spec.organization` to match. Only the documented spec/status
fields (plus model), name, namespace and resourceVersion are projected; arbitrary
annotations, desired payloads and before snapshots are excluded to avoid
exposing credentials in application configuration. No Secret resources are read.

No pagination in this minimal contract; the service must return the complete
organization result. Large installations should extend both sides together.

### GET /v1/organizations/{org}/compliance

Requires developer, platform, admin or auditor of that organization. Approver
alone does not grant access. Roles come from verified `aep-<org>-<role>` groups
or `/aep-<org>-<role>` groups or the Organization's configured external groups.
These external-group mappings apply only to compliance; `/v1/me` retains its
canonical-group contract. A missing Organization returns 403.
Missing/invalid token returns 401; missing membership returns 403; inaccessible,
invalid or wrong-owner snapshots return 503. Responses are `Cache-Control: no-store`.

```json
{
  "organization": "acme",
  "items": [{
    "tier": "prod",
    "stale": false,
    "snapshot": {
      "tier": "prod",
      "collectedAt": "2026-11-25T12:00:00Z",
      "policies": {"aep-prod-images": {"pass": 24, "fail": 1, "warn": 0, "error": 0, "skip": 0}},
      "failingResources": [{"policy": "aep-prod-images", "rule": "digest-only", "apiVersion": "v1", "kind": "Pod", "namespace": "dp-prod", "name": "orders"}],
      "failuresTruncated": false
    }
  }]
}
```

Each physical tier has an item. Before first collection its snapshot is `null`
and `stale` is true. Snapshots older than six minutes are stale. The operator
refreshes them every five minutes; the read API never contacts tier clusters.
Counts are per-policy result counts. At most 50 unique failing resource/rule
entries are returned in deterministic policy/resource order. Empty results do
not assert compliance. The portal Compliance page at `/aep/compliance` displays
per-tier cards, stale/unknown status, counts and failing resources, and refreshes
every minute. Switching organization cancels display of the previous response.

Snapshots are read from `--namespace` (default `aep-system`), which must match
the operator namespace. The `charts/aep-read-api` chart configures this through
`complianceNamespace` and grants ConfigMap get access only in that namespace.

## Existing change API

`aep.changeApiUrl` is the change service base URL. The proxy preserves upstream
status codes and ETag. It allows only these routes:

- `POST /v1/changes`: JSON `{organization, operation, model, target, desired,
  justification}`, with `Idempotency-Key`. Requester is set by the change API.
- `GET /v1/changes/{org}/{name}`: returns `{id, request: ChangeRequest}`.
- `POST /v1/changes/{org}/{name}/decision`: JSON `{decision: "approve"|"reject"}`,
  with `If-Match` equal to the freshly fetched `metadata.resourceVersion`.

Creation and decision responses use `{id, request: ChangeRequest}`. On 412 the
UI clears detail and requires another read before a decision. Network retries
of an unchanged creation retain the same key; editing input creates a new key.
Closing and reopening the dialog starts a new intent. Check the existing
request before reopening after an uncertain response.

The dialog currently exposes the operation payload as JSON. Promote/rollback
use `{"releaseName":"..."}`. Resize (`resource-update`) accepts
`resourceRelease`, `resourceTypeEnvironmentConfigs`, and/or `retainPolicy`,
matching `internal/change/validate.go`. Restore payloads depend on the operation
handler. Password rotation can use `{}`. No password is returned to the UI.

## Local mock

From `portal`, run `yarn mock` (or the documented npx equivalent). It binds only
127.0.0.1:7100, accepts `Bearer demo-requester` and `Bearer demo-approver`, and
contains organization `acme`. It models creation, idempotency conflicts,
independent approvals, optimistic concurrency and list/detail reads. State is
in memory and disappears on restart. Run `node --test dev/mock.test.mjs`.

These opaque demo tokens are for direct API/proxy tests. Browser sign-in still
requires Keycloak and real API implementations accepting its tokens; no guest
provider or development authentication bypass is shipped in the portal.
