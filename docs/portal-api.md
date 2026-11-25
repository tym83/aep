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

## Read API (new integration contract)

`aep.readApiUrl` is an origin/base path without `/v1`. These endpoints need an
implementation backed by bridge/ServiceClaim and ChangeRequest state. They are
not implemented by the current Go change API. The portal includes a development
mock, not a production read service.

### GET /v1/me

```json
{"username":"alice","organizations":[{"name":"acme","roles":["developer","approver"]}]}
```

Return only accessible organizations, using their DNS-label names. The portal
selects the first organization by default and offers a selector for others.
Role `approver` enables the Changes page for that organization.

### GET /v1/organizations/{org}/services

Return only applications created by the AEP bridge for this organization,
including tenant-environment clusters and managed services. Do not enumerate
arbitrary Cozystack applications or return credentials.

```json
{
  "items": [{
    "name": "orders-db-prod",
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
        "type": "Ready", "status": "True", "observedGeneration": 2,
        "reason": "Reconciled", "message": "Application is ready"
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
HTTP(S) console link. Conditions come from ServiceClaim status, including
`observedGeneration`. A current `Degraded=True` wins over `Ready=True`; a current
`Ready=True` is Ready; everything else (including stale or absent conditions)
is Updating. The message/reason is displayed without HTML interpretation.

### GET /v1/organizations/{org}/changes

Requires organization `approver`. Returns `{"items": [ChangeRequest, ...]}`,
using the existing `aep.aenix.io/v1alpha1` object shape. Include metadata `name`
and `resourceVersion`, spec `organization`, `requester`, `operation`, `target`,
`justification`, optional `window`, and status `phase` and `approvals`.
No pagination in this minimal contract; the service must return the complete
organization result. Large installations should extend both sides together.

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
