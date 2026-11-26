# Audit pipeline

`cmd/audit-forwarder` runs on the management cluster. It watches ChangeRequests
and Organizations, accepts Kubernetes `audit.k8s.io/v1 EventList` batches at
`POST /kubernetes`, and accepts structured OpenChoreo audit records at
`POST /openchoreo`. All accepted events enter one persistent, hash-chained stream
before delivery to every configured sink. This implements the export component
of DESIGN §5.13, not the whole scenario 7 acceptance test.

## Event contract and verification

The JSON Schema is [event-v1.schema.json](../config/audit/event-v1.schema.json).
Each line of an export is one `aep.audit/v1` event:

| Field | Meaning |
| --- | --- |
| version | Schema and hashing protocol version; incompatible changes require v2 |
| stream, sequence | Stable installation stream ID and monotonically increasing number |
| id | Source event ID for semantic deduplication; retries can repeat it |
| time, recordedAt | Source time (or observation time), and durable ingestion time, RFC3339Nano |
| source, action | Source subsystem and operation/lifecycle event |
| actor | Source identity; `unknown` for Organization watch observations |
| organization, resource | Organization when available and resource identity |
| details | Source-specific evidence described below |
| prevHash, hash | Previous record SHA-256 (empty at genesis), current SHA-256 |

Hash algorithm: set `hash` to the empty string, then SHA-256 the UTF-8 output of
Go `encoding/json.Marshal(Event)` without a newline. Struct field order is
`recordedAt,version,stream,sequence,id,time,source,action,actor,organization,resource,details,prevHash,hash`;
empty organization/details are omitted. Nested map keys are sorted by Go's JSON
encoder; normal HTML escaping applies. This is a versioned Go encoding contract,
not RFC 8785. Use the supplied verifier rather than reserializing with a different
JSON encoder. Sequence numbers and hash continuity are both checked.

```sh
go build -o bin/audit-forwarder ./cmd/audit-forwarder
bin/audit-forwarder verify export.jsonl
bin/audit-forwarder verify --prev-hash="$TRUSTED_PREDECESSOR" \
  --last-hash="$TRUSTED_FINAL_HASH" day.jsonl
```

Preserve final hashes in an independently protected system. A chain alone cannot
detect wholesale replacement or tail truncation without a trusted checkpoint;
it is tamper evidence, not a signature. Empty exports are rejected. S3 retries
can produce overlapping chunks: assemble one stream by sequence, deduplicate
identical `(stream, sequence, hash)` records, reject conflicting hashes, then
verify. Do not concatenate different streams or sort by source timestamp.

Change events include `changerequest.created`, `.decided`, `.applied`, `.failed`
and `.observed` (status/notes/other transitions). Details carry requester,
approvals with approver/evidence, notes, phase, target, conditions and the before
snapshots. `beforeSnapshotRef` identifies CR UID, resourceVersion and a content
hash; the snapshot is also embedded so deleting the CR does not erase it.
First observation reconstructs available lifecycle history; `recordedAt` makes
that distinction visible. Stable lifecycle IDs suppress repeats across watch
updates and restarts. `.applied` and `.failed` represent first observation of
those phases, not every apply attempt. Other phase changes remain in `.observed`.
Organization events contain UID, resourceVersion, generation and a spec hash;
they do not invent a human identity. Correlate their UID/resourceVersion with
Kubernetes API audit records for the actual writer and authorization result.

Kubernetes records retain user/groups, impersonated user, object reference,
response status, stage and authorization annotations. Request/response bodies
are deliberately excluded. Filtering accepts only `aep.aenix.io` and
`apps.cozystack.io`; this receiver does not collect workload-cluster audit.
Snapshots, approval evidence and notes can contain confidential information:
restrict SIEM access and redact at the producer where necessary.

## Persistence and sink behavior

The PVC contains an atomic `state.json` with pending events, chain head and watch
observation checkpoints. Every append writes a mode-0600 temporary file, fsyncs,
renames and fsyncs the directory before returning HTTP 204. A process-level lock
prevents two writers. Run one replica with `Recreate` and a filesystem providing
POSIX rename/fsync/flock; do not share the PVC between forwarders. Preserve the
PVC across upgrades/uninstalls and keep the stream name stable. A lost PVC needs
a new stream ID and an explicit gap report; never silently reset a stream.

The state defaults to 64 MiB, including observation checkpoints. Provision at
least twice that size for atomic rewrites plus filesystem overhead. This simple
bounded spool rewrites its state on each append/ack; it is intended for stage-1
volumes, not high-throughput logging. Checkpoints for deleted objects persist;
monitor growth and enlarge capacity before exhaustion. No automatic pruning of
identity history is implemented.

Each export sends at most 100 events to each sink. Only after all sinks succeed
is the batch acknowledged locally. Failures retry after one second, preserving
order; a failed sink blocks all progress. Successful sinks can receive duplicates.
The receiver returns 503 with Retry-After on capacity/storage failure. Watches
stop consuming until persistence succeeds. Once admitted to the spool, events
are at-least-once subject to PVC durability. Watches are observations, not a
complete audit log: relisting after watch expiry cannot reconstruct deleted
objects or intermediate states. API-server audit must fill this gap.

- **OpenSearch:** HTTPS `_bulk`, NDJSON, stable document ID = chain record hash.
  HTTP and per-item failures prevent acknowledgment. Credentials are
  `OPENSEARCH_USERNAME` / `OPENSEARCH_PASSWORD`. Provision index permissions and
  a 30-day ISM retention policy separately; the forwarder never administers it.
- **Syslog:** TLS 1.2+, RFC5424 with RFC6587 octet framing, local0/info, JSON MSG.
  Configure `--syslog-address=host:6514`; system CA roots verify the server.
  TCP/TLS confirms transport only: RFC5424 offers no durable application ACK.
  Receiver-side loss after a successful write is not covered. Use S3/OpenSearch
  in parallel for durable acknowledgment.
- **S3:** HTTPS; `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optional
  `AWS_SESSION_TOKEN`. Keys are `prefix/stream/YYYY-MM-DD/first-last-hash.jsonl`,
  partitioned by UTC ingestion day. These immutable chunks form the daily export;
  no append/overwrite of a single daily object is required. Enable versioning,
  Object Lock and default compliance retention on the bucket **before use**.
  Default retention is inherited; the writer needs PutObject, not DeleteObject
  or retention-bypass permissions. Retried PUTs can create additional locked
  versions. Bucket encryption/KMS and retention are deployment responsibilities.

## Installation and monitoring

Build `Dockerfile.audit-forwarder`, publish through the images workflow, and
supply its digest to `charts/aep-audit-forwarder`. The default all-zero digest is
an explicit non-deployable placeholder, not a published artifact. Provide:

- `aep-audit-tls`: `tls.crt` and `tls.key`, with Service DNS in the certificate SAN.
- `aep-audit-token`: `token`, at least 32 random characters.
- `aep-audit-sinks`: environment variables for configured sinks (may be empty
  for anonymous authenticated-by-network installations).
- At least one sink URL/address; configure S3 and SIEM in parallel for production.

```sh
helm upgrade --install aep-audit charts/aep-audit-forwarder -n aep-system \
  --set image="$AUDIT_IMAGE_DIGEST" --set opensearchURL=https://search.example \
  --set s3Endpoint=https://s3.example --set s3Bucket=aep-audit
```

The chart grants only list/get/watch on the two AEP resources. Pods satisfy
restricted Pod Security, and carry Cozystack's API-access label. Restrict ingress
to API servers/log shippers and egress to DNS, Kubernetes and configured sinks
using site network policies. Pin Tier-0 nodes with nodeSelector/tolerations.
Private CA bundles can be mounted by a deployment overlay and selected with
`SSL_CERT_FILE`; no TLS verification bypass is provided. Tokens/certificates are
loaded at startup; roll the pod on rotation.

`GET /healthz` is process health, not successful export. `/metrics` exposes
`aep_audit_pending_events` and `aep_audit_oldest_pending_seconds` over HTTPS.
Scrape internally; alert at 240 seconds and page at 300 seconds to enforce the
≤5-minute SIEM target. Also alert on PVC utilization, receiver 503s, export retry
logs and source audit backend failures. On backlog: fix the failing sink, verify
head continuity, watch the oldest pending age fall; never delete the spool to
clear an alert. These metrics do not prove that upstream sources are emitting:
use a synthetic API write and confirm its identity in every sink.

## Kubernetes audit configuration: Kamaji and Cozystack

The example [policy](../config/audit/policy.yaml) uses Metadata for the two
management API groups. Mount it with the [webhook kubeconfig](../config/audit/webhook-kubeconfig.example.yaml),
CA and token in a Secret named `aep-audit-apiserver` in the **management namespace
hosting the control-plane pods**. Keys: `policy.yaml`, `webhook.yaml`, `ca.crt`,
`token`. Restrict who can read this Secret.

Kamaji has two different API layouts; do not confuse them:

| Object | Arguments | Volumes | Mounts |
| --- | --- | --- | --- |
| TenantControlPlane | spec.controlPlane.deployment.extraArgs.apiServer | spec.controlPlane.deployment.additionalVolumes | spec.controlPlane.deployment.additionalVolumeMounts.apiServer |
| KamajiControlPlane (Cluster API provider) | spec.apiServer.extraArgs | spec.deployment.extraVolumes | spec.apiServer.extraVolumeMounts |

See the [Kamaji API reference](https://kamaji.clastix.io/reference/api/) and
[TenantControlPlane example](../config/audit/kamaji.example.yaml). Append the
example flags and mounts to existing arrays. Cozystack/CAPI reconciles these
objects: a direct TCP patch is not a persistent installation method.

The inspected local Cozystack `packages/apps/kubernetes` checkout exposes
`controlPlane.apiServer.extraArgs`, `.extraVolumes`, `.extraVolumeMounts` through
its chart to KamajiControlPlane. The same example becomes:

```yaml
controlPlane:
  apiServer:
    extraArgs:
      - --audit-policy-file=/etc/aep-audit/policy.yaml
      - --audit-webhook-config-file=/etc/aep-audit/webhook.yaml
      - --audit-webhook-version=audit.k8s.io/v1
      - --audit-webhook-mode=batch
      - --audit-webhook-batch-max-wait=5s
    extraVolumes:
      - name: aep-audit
        secret: {secretName: aep-audit-apiserver}
    extraVolumeMounts:
      - {name: aep-audit, mountPath: /etc/aep-audit, readOnly: true}
```

This local capability is **not evidence that the installed Cozystack 1.7.x
package exposes it**. Check the shipped values schema and rendered KCP/TCP
before rollout. If absent, the upstream item is to expose and validate these
three fields through the Kubernetes application API/chart and preserve them
through reconciliation. Do not patch the generated Deployment. Configure Talos
management API-server audit separately through the supported Talos machine
configuration; tenant flags do not audit management-cluster writes. That
management-cluster enablement remains a site/upstream Cozystack integration item.

Batch mode may lose in-memory events during API-server failure or overflow; alert
on its audit backend errors. Stronger blocking modes change API availability
and require separate testing. On a staging cluster, validate a write by a known
user reaches the receiver with correct auditID/user/objectRef and survives a
sink outage. No live cluster validation was performed for this implementation.

## OpenChoreo coverage and remaining gaps

[OpenChoreo's audit documentation](https://openchoreo.dev/docs/platform-engineer-guide/audit-logging/)
describes structured audit records for API writes, including denied results,
with actor/resource identity and correlation IDs. It documents `AUDIT-LOG`
collection through observability modules and warns that the format is beta.
Unauthenticated requests and ordinary reads are not covered. Role changes are
audited; this does not imply that every internal authorization evaluation emits
an independent event.

Configure a durable log shipper to extract the JSON audit record and POST it to
`/openchoreo` with the bearer token, retrying non-2xx responses. The adapter
accepts `event_id`, `event_time`, `action`, `result`, `actor`, `resource` and
`request_id`. A shipper deployment is not included. Verify the emitted schema
against the stand's pinned **1.4.0-m.3** before enabling: current documentation
is not a tag-specific emission test. Older wiring gaps are tracked in
[upstream issue 3343](https://github.com/openchoreo/openchoreo/issues/3343).
Do not claim OpenChoreo coverage until a real allowed and denied API write both
arrive; record any schema/middleware gap against that pinned version.

Not implemented here: Keycloak/Argo CD ingestion, production-change/approval
correlation alerts, CEF/OTLP/Kafka sinks, signatures, automatic source gap repair,
per-organization routing, SIEM provisioning, and end-to-end scenario 7 tests.

S3 signing uses `AWS_REGION` (default `us-east-1`); set it to the bucket region.
The configured region avoids requiring bucket-location discovery permissions.
