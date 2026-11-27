# ITSM bridge

`cmd/itsm-bridge` mirrors AEP ChangeRequests to ServiceNow, Jira Service
Management, or a signed JSON webhook. It forwards approval evidence through
`itsm/contract/openapi.yaml`; execution remains owned by the change controller.
The ServiceNow adapter also sends CSDM graphs through IRE and alerts through
Event Management. All tests run offline with fake vendor HTTP servers.

## Approval boundary

Design §5.6 explicitly chooses human confirmation, so this implementation does
**not** add service-account decisions or trust ITSM-supplied groups. A CAB/ECAB
approval arrives as `change.note` evidence, including the immutable corporate
approver ID and external approval reference. It does not approve AEP. For
ServiceNow the bridge reads `sysapproval_approver` decisions on mirrored
pending changes itself and posts each approved or rejected record once; ITSM
automation may also send signed `change.note` events inbound.

The approver follows `confirmationUrl/<organization>/<change>` to the existing
AEP approval UI, signs in with Keycloak, reviews the current change, and posts
`/v1/changes/<organization>/<change>/decision` with their own bearer token,
`If-Match: <reviewed resourceVersion>`, and the external reference as `evidence`.
The change API enforces eligible groups, separation of duties, Pending phase,
and freshness; the controller checks approval eligibility again. A stale
version requires another review. These confirmations retain `source: portal`;
ITSM evidence notes have `source: itsm`.

Configure CAB users as `aep-<org>-approver` and ECAB users as
`aep-security-oncall` through the corporate identity provider. The bridge never
converts ITSM group names to authorization. The approval UI must already exist
at the configured URL; this change does not add a portal route. Signed IdP
assertion exchange, automatic directory-ID mapping, ordered CAB quorum, and the
cross-system audit reconciler are not implemented. They are not bypassed by a
webhook or a bot token.

## Running and credentials

Run one active process per organization and adapter. The chart fixes one
replica and uses `Recreate`; do not start a second process on the same state.
Use a dedicated namespace per organization. A configuration example:

```json
{
  "organization": "acme",
  "changeNamespace": "tenant-acme",
  "stateNamespace": "itsm-acme",
  "adapter": "servicenow",
  "vendorUrl": "https://dev123.service-now.com",
  "tokenUrl": "https://dev123.service-now.com/oauth_token.do",
  "clientId": "aep-itsm-acme",
  "aepUrl": "https://aep-change.example",
  "aepTokenUrl": "https://id.example/realms/aep/protocol/openid-connect/token",
  "aepClientId": "aep-itsm-acme",
  "confirmationUrl": "https://portal.example/changes",
  "syncCmdb": true,
  "standardTemplate": "published_standard_template_sys_id",
  "companySysId": "existing_core_company_sys_id",
  "services": [{
    "uid": "corporate-catalog-orders-prod-uid",
    "name": "orders",
    "environment": "prod",
    "tier": "prod",
    "databases": ["tenant-acme-prod/Postgres/aep-example-ordersdb"],
    "businessApplication": "customer_business_application_sys_id"
  }]
}
```

`services` supplies stable service/environment identities and explicit database
dependencies from the customer's catalog. Replace every example identity with
real inventory values. Omit `businessApplication` until the customer supplies
an existing sys_id. The bridge discovers organization UID, actual tier cluster
UIDs, and bridge-managed Postgres/Redis UIDs from management-cluster APIs. It
does not discover OpenChoreo component dependencies automatically or read
connection Secrets. A complete successful inventory retires missing owned CIs;
failed lists or unresolved dependencies prevent publishing a partial snapshot.

Supply credentials through the existing Secret referenced by
`credentialsSecret`, or environment variables for local execution:

| Variable | Purpose |
|---|---|
| `ITSM_INBOUND_HMAC_SECRET` / `ITSM_INBOUND_HMAC_KEY_ID` | At least 32 random bytes and a nonempty key ID for trusted inbound automation. |
| `ITSM_INBOUND_HMAC_PREVIOUS_SECRET` / `ITSM_INBOUND_HMAC_PREVIOUS_KEY_ID` | Optional previous inbound key during rotation; remove after senders switch. |
| `ITSM_OUTBOUND_HMAC_SECRET` / `ITSM_OUTBOUND_HMAC_KEY_ID` | Separate 32-byte key and ID for the generic outbound webhook; must differ from inbound. |
| `ITSM_CLIENT_SECRET` | ServiceNow OAuth client secret. |
| `ITSM_TOKEN` | Jira scoped OAuth token, or API token when `jiraEmail` enables Basic email:token authentication. |
| `AEP_CLIENT_SECRET` | Keycloak organization service-account secret; only used to read/comment, never decide. |

The AEP client needs an organization membership group for the existing notes
API, for example `aep-acme-developer`. Tokens must use the change API audience.
ServiceNow and AEP client-credentials tokens are cached and refreshed by the
OAuth transport. Rotate environment-backed credentials by restarting the pod.
Outbound redirects are rejected. HTTPS is required unless `--allow-http` is
explicitly used with local fakes. `--tls-cert` and `--tls-key` enable inbound
TLS; otherwise use a trusted private TLS gateway. HMAC verification is mandatory.

```sh
go build -o bin/itsm-bridge ./cmd/itsm-bridge
bin/itsm-bridge -h
bin/itsm-bridge --mgmt-kubeconfig=/secure/management.conf --config=itsm.json
helm lint --strict charts/aep-itsm-bridge -f charts/ci/itsm.yaml
helm template itsm charts/aep-itsm-bridge -n itsm-acme -f charts/ci/itsm.yaml
```

The Helm CI fixture has a **nonexistent rendering-only digest**. For installation,
provide the image digest published by `.github/workflows/images.yml`, real
configuration, and a pre-created credentials Secret. The chart rejects missing
or malformed digests. No bridge image is published by this branch. Pods run as
UID 65532, without capabilities or privilege escalation, with RuntimeDefault
seccomp and a read-only filesystem. RBAC reads changes in the selected namespace,
reads one Organization, lists tier applications, and writes ConfigMap cursors in
the bridge namespace. It cannot write ChangeRequests or execute bindings.
Install into a **dedicated bridge namespace**, not a shared application namespace:
Kubernetes RBAC cannot restrict create/list by label or name prefix. Dynamic
hashed ConfigMap names prevent an exhaustive resourceNames grant. The namespace
is the write boundary; only bridge configuration and cursors belong there.
Cursor names start with `itsm-` and reads/retries filter organization labels.
Set `config.mirrorSince` (RFC3339) to the moment the integration is enabled:
changes created earlier are not mirrored, so an existing organization does not
flood the ITSM with its history. Empty mirrors every change.
Set `tiers` in chart values to the actual Organization tier names (no built-in
tier list). `config.syncCmdb: null` selects true for ServiceNow and false for
Jira/webhook; an explicit boolean overrides it.

On the stand, root ingress to `aep-system` needs the explicitly configured
Cozystack network-policy path recorded in the session log. This chart creates
no public ingress and assumes no cross-tenant network reachability.

## ServiceNow setup and mapping

Use a separate OAuth application/integration identity for each organization.
Enable the instance's client-credentials grant and configure the application
user; the token endpoint is `/oauth_token.do` ([client credentials workflow](https://www.servicenow.com/docs/r/fr-FR/platform-security/authentication/client-credentials-grant-workflow.html)).

Set `config.assignmentGroup` to the sys_id of the group that owns mirrored
changes: ServiceNow's state field policy keeps a normal change in New until it
has an assignment group, so without it the bridge cannot move the change.

Grant the minimum ACLs needed for `change_request` read/create/update and
`sysapproval_approver` read. IRE documents the `itil` or `asset` role, and Event
Management requires `evt_mgmt_integration`; validate all additional scoped-app
ACLs in the PDI. Do not give this identity administrative access or permission
to approve as another person. See the [IRE API](https://www.servicenow.com/docs/r/api-reference/rest-apis/c_IdentifyReconcileAPI.html)
and [Event Management API](https://www.servicenow.com/docs/r/it-operations-management/event-management/send-events-via-web-service.html).

Configure `AEP` as a discovery source, per-class identification rules for stable
source-native keys, reconciliation priority for owned fields, and permitted
relationship types. Add a unique constraint for the AEP `correlation_id`
namespace (or a scoped endpoint enforcing it). Lookup plus uniqueness makes
creation recoverable when the response is lost. Do not rely on the
`Idempotency-Key` header alone: the Table API does not promise that behavior.

| AEP | ServiceNow |
|---|---|
| Composite change ID | `correlation_id = aep-<sha256(id)>`; external sys_id retained in durable state. |
| `normal`, `standard`, `emergency` | Model-specific Change Management creation; standard uses configured `standardTemplate`. |
| Phase / confirmation link / short revision hash | Bounded `description`; operation and ID in `short_description`. No raw ChangeRequest or justification is sent. |
| Window | UTC `start_date`, `end_date`. |
| Approved / Applying | Scheduled (or Implement if already advanced) / Implement. |
| Applied | Implement → Review → Closed, `close_code=successful`. |
| Failed | Closed (`state=3`), `close_code=unsuccessful`, result in `close_notes`. |
| Rejected / Expired / Superseded | Canceled (`state=4`). |
| ITSM requested / approved / rejected | Read from `sysapproval_approver.state`; approved/rejected automation emits evidence only. |

The adapter uses the [Change Management API](https://www.servicenow.com/docs/r/api-reference/rest-apis/change-management-api.html):
`POST /api/sn_chg_rest/change/{normal|emergency}` or
`POST /api/sn_chg_rest/change/standard/{standardTemplate}` and model-specific
PATCH. It never rewrites type or grants ITSM approval. Before each state change,
`GET /api/sn_chg_rest/change/{id}/nextstates` supplies allowed states and guards;
unavailable transitions remain pending for CAB/business rules. Normal walks
Assess/Authorize, then Scheduled/Implement/Review as permitted. Table API is
used only for correlation lookup and approval evidence. Validate mandatory
fields and the published standard template on the target PDI.

### CSDM and IRE

| Neutral kind | ServiceNow class | Ownership |
|---|---|---|
| Organization | Existing `core_company` via `companySysId` | Not a CI; excluded from IRE and linked through Business Application `company`. |
| Application Service per service/environment | `cmdb_ci_service_discovered` | AEP-owned; Depends on cluster and database. |
| Tier cluster | `cmdb_ci_kubernetes_cluster` | Actual discovered cluster UID plus management source identity. |
| Managed Postgres/Redis | `cmdb_ci_db_instance` | Actual managed application UID; database-instance abstraction. |
| Business Application | `cmdb_ci_business_app` | Customer-owned; reference by existing sys_id only. |
| Technical Service / Service Offering | `cmdb_ci_service_technical` / `service_offering` | Supported in supplied graphs; catalog population is external. |

Class names are checked against ServiceNow's [Application Services documentation](https://www.servicenow.com/docs/r/servicenow-platform/configuration-management-database-cmdb/application-services.html),
[Kubernetes inventory mapping](https://www.servicenow.com/docs/r/yokohama/it-operations-management/discovery/cnov-collected-data.html),
and [CMDB table descriptions](https://www.servicenow.com/docs/r/servicenow-platform/configuration-management-database-cmdb/cmdb-tables-details.html).
Company references must identify an existing `core_company`; the bridge does
not create organizations as Application Services. Business Applications remain
customer-owned and are never retired.

Populate management application annotations `aep.aenix.io/itsm-k8s-uid` with
the actual discovered Kubernetes cluster UID (not the management CR UID),
`aep.aenix.io/itsm-fqdn` with the endpoint FQDN and `aep.aenix.io/itsm-port` with
the endpoint port. IRE includes cluster `k8s_uid` and database `instance_name`
(application name), `fqdn`, and `tcp_port`. Missing required identity rejects the
snapshot rather than producing name-only CIs. Management UID remains the AEP
source-native key. On the PDI verify OOB identification entries, attribute names,
and dependent identification/hosting rules for both classes against installed
CMDB class-model and Discovery versions. In particular, database identification
may require a hosting CI relation: set `aep.aenix.io/itsm-host-sys-id` to an
existing discovered `cmdb_ci_server` to include a reference-only host and
`Runs on::Runs` relation. Verify the appropriate host/class mapping before
enabling production sync; Kubernetes placement is not inferred. Do not substitute a
fabricated cluster UID or assume source-native keys satisfy initial OOB matching.
Use IRE identify-only previews to verify both first creation and reconciliation
with records from Discovery; confirm no duplicates and correct company ownership.

IRE payloads use `discovery_source=AEP`, `sys_object_source_info.source_name=AEP`,
and `source_native_key=uid`. Relationship endpoints are array indices:
`Contains::Contained by`, `Runs on::Runs`, `Depends on::Used by`, and
`Consumes::Consumed by`. Invalid or dangling relations are rejected before
sending. Retired owned CIs get `install_status=7`; Business Applications are
never retired. IRE item and relationship errors prevent cursor advancement.
Relationship deletion and CMDB Data Manager lifecycle configuration remain
instance responsibilities; dropping a relation from a payload does not delete it.

Alerts use `/api/global/em/jsonv2` (the `em_event` ingestion API), with
`source=AEP`, `message_key=<organization>/<alert key>`, node and description.
Severity is `1` critical through `5` OK; resolution sends `0` with the same key.
An Event Management license is required. Incident API fallback is not implemented.

## Jira Service Management

Set `adapter=jira`, `syncCmdb=false`, the Jira site `vendorUrl`, `jiraProject`,
`jiraServiceDesk`, and `jiraRequestType`. Use a scoped OAuth access token with
project browse/create/edit/transition permissions, service desk access, and
approval-read scopes. See [JSM request and approval endpoints](https://developer.atlassian.com/cloud/jira/service-desk/rest/api-group-request/)
and [Jira REST v3 issues](https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issues/).

The request type must expose `summary`, `description`, and **labels**, so the
correlation label is written atomically at creation. Configure and test this
request type before enabling the bridge. No follow-up label write is used.

| Neutral field | Jira |
|---|---|
| Change | `POST /rest/servicedeskapi/request`, configured service desk/request type. |
| Stable change ID | `aep-<sha256(id)>` label; issue key in durable state. |
| Current change view | Bounded summary; description is set only at creation and never overwritten. |
| Applied / Failed / other phases | Optional `jiraTransitions` map from phase to workflow transition ID. |
| Approval evidence | `GET /rest/servicedeskapi/request/{key}/approval`; never answered by the bot. |
| Raised alert | REST v3 issue of type `Incident`, correlated by alert-key hash. |
| Resolved alert | Update incident and optional `jiraTransitions["alert.resolved"]`. |

Configure an Incident issue type and `alert.raised` transition if resolved
incidents should reopen. Configure transitions for terminal change phases;
without them the fields update but the workflow status does not close.

Approval automation signs the same `change.note` envelope described below.
Human confirmation remains mandatory. Assets CMDB, JSM Operations routing,
request-type provisioning, and automatic token renewal are not implemented.
Jira search can lag indexing. After an ambiguous create, a persisted five-minute
index deadline prevents immediate re-creation; retries use capped exponential
backoff. Search requests `fields=key` and orders by `created ASC, key ASC` to
adopt the oldest match. Fakes verify delayed indexing and recovery after restart.
A create committed just before process death or indexing delayed beyond the
bounded window still requires an idempotent gateway for strict exactly-once
creation; Jira labels are not unique constraints.

## Generic webhook and inbound JSON

Set `adapter=webhook`, with `vendorUrl` pointing to the receiving URL. Every
outbound delivery is a structured CloudEvent defined in
`itsm/contract/events.schema.json`. The receiver must persist `id` before
acknowledging 2xx and deduplicate it for at least seven days.

Both directions use these headers:

- `X-AEP-Key-ID`: configured directional key ID; unknown IDs are rejected inbound.
- `X-AEP-Timestamp`: Unix seconds, accepted within five minutes.
- `X-AEP-Signature`: lowercase hex HMAC-SHA256 over `timestamp + "." + rawBody`.
- Outbound `Idempotency-Key`: event ID.

Inbound endpoint: `POST /v1/events`, maximum body 512 KiB. It accepts
`change.note`, `alert.raised`, and `alert.resolved` only. CMDB is outbound-only;
all inbound CMDB events and change/decision commands are rejected. Inbound
alerts are **trusted-sender events**: HMAC identifies authorized automation,
not independent evidence that an alert occurred. Alerts use event time to ignore
older updates within the receipt horizon. Native vendor webhooks must
be translated and signed by customer automation; they are not parsed directly.
No token, password, database connection secret, or arbitrary URL belongs in data.

```json
{
  "specversion": "1.0",
  "id": "servicenow-approval-immutable-record-version",
  "source": "aep",
  "type": "change.note",
  "subject": "change/acme/chg-example",
  "time": "2026-11-26T02:00:00Z",
  "datacontenttype": "application/json",
  "data": {
    "organization": "acme",
    "change": "chg-example",
    "text": "CAB approved; awaiting human confirmation in AEP",
    "externalId": "approval_sys_id",
    "approverId": "immutable-corporate-directory-object-id",
    "decision": "approve"
  }
}
```

`source=aep` identifies this normalized contract, including vendor automation
that produces AEP-compatible envelopes. Inbound subject is derived from the
organization and payload, not trusted from the request. Alert keys are scoped
by organization. Note text is limited to 3000 bytes, external/approver IDs to
255 bytes each, and rendered evidence text to 4000 bytes including JSON escaping.
Validation occurs before acceptance. Evidence is an external reference, never
confirmation. Outbound change data is `{id, text}` containing phase, confirmation
link (up to 1024 characters), and a 12-character hash. Jira summaries are limited
to 255 characters; ServiceNow short descriptions to 160. No raw ChangeRequest
JSON is sent to any vendor, including the generic webhook.

## Delivery, persistence, and limits

Each subject has a ConfigMap containing its pending event, external ID, retry
count/deadline, completed cursor, and seven-day event receipts. State is scoped
by organization, with separate AEP-note and vendor queues and retry workers.
Per-subject locks protect ordering; network I/O never holds the global lock.
Newer change/CMDB snapshots replace pending state while preserving Ref; alerts
also reject older event times. Retries back off from 2 to 256 seconds. Permanent
4xx (except 408/429) are stored as terminal failures and never block later events.
Inbound permanent AEP failures return the same 4xx on replay; transient failures
return 503. Reuse the same event ID/payload when retrying.

`GET /metrics` exposes `aep_itsm_records{status="pending|completed|failed|corrupt"}`.
Record JSON retains the terminal HTTP status in `failure` and receipt `status`.
Completed records are removed after the seven-day receipt TTL; corrupt records
are reported and skipped so other deliveries can progress. Confirmed retired CIs
are omitted from the next snapshot. Keep terminal ChangeRequests until mirrored.
The store caps each record at 700 KB and 2,000 receipts per seven days; it is not
an audit archive. Delayed replay protection is bounded by the receipt TTL.

Remote effects and Kubernetes persistence are not a distributed transaction.
ServiceNow needs its correlation uniqueness rule; generic receivers need their
own event deduplication. IRE uses native source identity and Event Management
uses message-key correlation. State has no human tokens or integration secrets,
but may contain change justification/evidence, so protect and back it up as
operational data. The local receipt store is not an audit archive.

## Validation and PDI follow-up

```sh
go build ./... && go vet ./... && go test ./...
go test ./internal/itsm -run TestScenario3ServiceNow -v
python3 scripts/itsm/servicenow-pdi.py --help
```

The scenario test uses the real AEP change API and controller, fake Kubernetes,
a fake OpenChoreo binding API, and fake ServiceNow. It covers Component and
Resource promotions, signed CAB evidence, replay, self/bot denial, stale
If-Match, human approval, applied bindings, ServiceNow closure, standard
rollback without CAB, and an IRE graph. Separate tests validate inventory
relationships/retirement, retry recovery, OAuth, signatures, and Jira behavior.
Actual deployment of workloads and a database remains the downstream
OpenChoreo/bridge responsibility and is not simulated as real infrastructure.

For a later PDI run, configure the instance and bridge, create an AEP request,
and export `SN_URL`, `SN_ACCESS_TOKEN`, `AEP_URL`, and `AEP_HUMAN_TOKEN` without
putting token values in command history. Run a read-only correlation check:

```sh
python3 scripts/itsm/servicenow-pdi.py --change acme/chg-example
```

After a human approves the matching record in ServiceNow, reviews the AEP
change, and has a fresh eligible Keycloak token, run:

```sh
python3 scripts/itsm/servicenow-pdi.py --change acme/chg-example \
  --approval APPROVAL_SYS_ID --execute
```

The script treats evidence as a reference, not a confirmation. It verifies that
the external approval belongs to this change, then submits
the human decision with If-Match, and waits for Applied and successful closure.
It never changes Kubernetes directly. Inspect the IRE graph in the PDI for the
core_company reference, configured tier clusters, service/environment, database,
Business Application link, and all configured relations; verify retirement and
business rules as a separate PDI acceptance check.

No real ServiceNow/Jira instance was used. A certified Service Graph Connector,
scoped app/MID pull transport, Store certification, catalog request endpoints,
paginated snapshot/replay API, and reconciliation of ITSM decisions against the
audit ledger remain separate work. This bridge is the testable adapter runtime,
not the certified stage-2 ServiceNow distribution. Customer instances must settle
Standard Change Templates, transition rules, identity mapping, CSDM relationship
policy, license availability, and the existing confirmation UI URL before rollout.

Decommission accepts an optional boolean `request.spec.desired.deleteData` for
Resource targets. `true` requires a normal, human-approved change and switches
the binding to `retainPolicy: Delete`. Omitted or false preserves Retain data.
Applied condition reasons `Retained` and `Finalizing` do not mean binding removal;
only confirmed absence permits owner cleanup. Desired payloads remain inside AEP
and are not exported to the vendor boundary.
