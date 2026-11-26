# aep-cp backup and disaster recovery

DESIGN §0.1 scenario 8 and §0.2 target **RPO ≤15 minutes, RTO ≤4 hours**, with
running services uninterrupted and no rollback of prod to stale desired state.
This package supplies a logical OpenChoreo export/restore tool. It does **not**
yet satisfy the complete no-disruption DR acceptance criterion.

## Critical distinction: logical export versus disaster recovery

`aep-cp` is a Cozystack tenant Kubernetes cluster with a Kamaji control plane.
Its etcd datastore is shared with other tenant control planes. DESIGN §5.2
requires UID-preserving recovery because OpenChoreo uses object UIDs in rendered
labels/selectors. Kubernetes manifest creation allocates new UIDs; the portable
restore implemented here cannot preserve them. Connecting live production agents
immediately after this restore could change immutable selectors or reconcile
stale state. Long-lived agent CAs solve authentication, not UID continuity.

Never restore an entire shared etcd snapshot over a running shared datastore to
recover one tenant. A tenant-prefix-aware, UID-preserving procedure needs a
separately validated Cozystack/Kamaji datastore backup workflow and disaster
exercise. It is outside this tool. Until that workflow or an OpenChoreo UID
migration/reconciliation procedure passes testing, scenario 8 remains open.

## Archive contract

`cmd/aep-cp-backup` uses management Organizations (cluster-scoped names) as the
namespace allowlist, or an explicit `--organizations=acme,example` list. On aep-cp
it discovers preferred served APIs in `openchoreo.dev` and subgroups ending in
`.openchoreo.dev`, including new kinds, and exports:

- Organization Namespace objects and all non-ServiceAccount-token Secrets in
  those namespaces, including Secret data/type/immutable settings.
- Every listable namespaced OpenChoreo kind in those namespaces.
- Every cluster-scoped OpenChoreo object, including shared planes, types and
  authorization resources. These may affect organizations outside the allowlist;
  use this archive only for full control-plane recovery, not tenant import.

OpenChoreo discovery/list errors fail the whole export. Unrelated partial discovery failures are tolerated. Organization namespaces not yet provisioned on aep-cp are recorded as absent. Runs (any kind ending in Run), Resource and RenderedRelease are excluded; objects with a controller ownerReference and ServiceAccount tokens are recorded as skipped.
Pagination is supported. UID, resourceVersion, timestamps, generation,
managedFields, status, ownerReferences and finalizers are stripped. Namespace
labels/annotations and desired object fields remain. Old owner UIDs/finalizers
are deliberately not replayed; verify controller reconstruction during drills.

The S3 object is an age-encrypted envelope around a single JSON archive (`aep.cpbackup/v1`), with a `payload`
containing created time, organization names and sorted `entries`. Each entry
contains a GVR, sanitized Kubernetes object and SHA-256 of its Go JSON encoding.
The envelope SHA-256 covers the complete payload, including scope and timestamp.
Restore validates every checksum, version, identity and namespace before writes;
checksums detect corruption, not malicious replacement. Keep S3 versions and
trusted hashes independently. Downloads are limited to 256 MiB; archives are
assembled in memory. This is not an atomic cross-resource Kubernetes snapshot:
freeze changes for a consistent export and reconcile any changes made during
or after its collection window.

Secrets make the archive highly sensitive. Require HTTPS, bucket encryption,
KMS policy, versioning and Object Lock default retention. Backup credentials
need PutObject only on the backup prefix; restore uses separate GetObject (and
GetObjectVersion when selecting a version) credentials. Neither role needs
DeleteObject or retention bypass. CA private keys held on the management cluster
are **not** part of this archive. Protect and back up that cluster independently.

## Schedule, credentials and commands

Build/publish `Dockerfile.cp-backup` and set its actual digest in
`charts/aep-cp-backup`; the all-zero default is an explicitly non-deployable
placeholder. The CronJob runs on the management cluster every ten minutes with
Forbid concurrency. Schedule plus backup duration and upload lag must remain
below 15 minutes; the schedule alone is not an RPO guarantee. Alert on the age of
the last successful archive (warning 12 minutes, critical 15), failed jobs and
jobs nearing their deadline. Measure completion duration and inventory counts.

Create `aep-cp-backup-kubeconfig` with key `config`, containing a **dedicated
read-only aep-cp identity**, and `aep-cp-backup-s3` with `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, optionally `AWS_SESSION_TOKEN`. The chart's management
ServiceAccount can only list Organizations. On aep-cp, grant discovery access,
get on selected Namespace names, namespace Roles listing Secrets and each
namespaced OpenChoreo resource, and a ClusterRole listing explicitly enumerated
cluster-scoped OpenChoreo resources. Generate/review the resource list from the
pinned OpenChoreo release; do not grant wildcard resources or admin credentials.
New discovered kinds without permission fail closed until reviewed. Restore
needs a separate temporary identity with get/create/update on these resources;
the CronJob does not carry write permissions. Credentials are mounted as a
whole directory to permit rotation between jobs.

```sh
go build -o bin/aep-cp-backup ./cmd/aep-cp-backup
helm upgrade --install aep-cp-backup charts/aep-cp-backup -n aep-system \
  --set image="$BACKUP_IMAGE_DIGEST" \
  --set s3Endpoint=https://s3.example --set s3Bucket=aep-dr \
  --set ageRecipient="$AGE_RECIPIENT"

bin/aep-cp-backup --mgmt-kubeconfig=mgmt-readonly.conf \
  --aepcp-kubeconfig=cp-readonly.conf \
  --age-recipient="$AGE_RECIPIENT" \
  --s3-endpoint=https://s3.example --s3-bucket=aep-dr backup
```

Successful output prints the object key, payload SHA-256 and object count.
Default keys include UTC date, nanosecond time and checksum; do not reuse an
explicit `--object` key for unrelated backups. Retention comes from the bucket.
`S3_VERSION_ID` selects an immutable object version during restore.

## Recovery runbook

1. **Declare the incident and freeze writes (budget 0–30 minutes).** Record the
   loss time and latest successful backup age. Stop promotion/change execution,
   GitOps writes and the AEP operator while rebuilding. Keep application data
   planes and agents running, but prevent them connecting to a replacement
   gateway. Protect bridges against deletions through the site's incident
   controls; this tool does not implement a global bridge pause switch. Preserve
   SIEM/audit history after the snapshot. App workloads should keep serving;
   verify that with probes before making recovery changes.
2. **Create the replacement tenant cluster (30–90 minutes).** Recreate `aep-cp`
   from the recorded Cozystack Kubernetes application values and supported
   Kubernetes version. Check capacity, service/pod CIDRs, API access and DNS.
   On the stand this was `aepcp` in `tenant-aepdev-cp`, Kubernetes v1.35; these are
   examples, not hard-coded product values. Use the supported Cozystack/Kamaji
   provisioning path. Do not touch the shared etcd datastore for logical restore.
3. **Reinstall the pinned OpenChoreo release (90–120 minutes).** Use the same
   archived Helm values, chart/images, authz/OIDC settings, CRDs, gateway TLS
   names and portal configuration. Keep OpenChoreo controller-manager and
   cluster-gateway replicas at zero from installation onward; check the pinned
   chart's values for those deployments. Do not merely scale down after agents
   have connected. Restore IdP/portal databases separately where required.
   The Cozystack cert-manager namespace is `cozy-cert-manager`; use its existing
   addons rather than installing conflicting copies. Verify bootstrap role
   bindings do not reintroduce broad developer/admin grants.
4. **Verify and plan the restore (120–150 minutes).** Choose the last valid
   immutable S3 version, verify its recorded checksum and age, then run:

   ```sh
   bin/aep-cp-backup --aepcp-kubeconfig=cp-recovery.conf \
     --s3-endpoint=https://s3.example --s3-bucket=aep-dr \
     --identity-file=recovery.agekey --object="$ARCHIVE_KEY" --dry-run --diff restore
   ```

   Dry-run/diff only GETs and LISTs target objects; it prints create/update/unchanged,
   resource identities and before/after hashes, never Secret bodies. It is a
   client-side plan, not admission-server validation. All archived API kinds and
   scope must match target discovery. Review global authz objects against the
   installed release before applying.
5. **Apply while reconciliation stays stopped (150–180 minutes).** After the
   UID reconciliation risk has been assessed:

   ```sh
   bin/aep-cp-backup --aepcp-kubeconfig=cp-recovery.conf \
     --s3-endpoint=https://s3.example --s3-bucket=aep-dr \
     --identity-file=recovery.agekey --object="$ARCHIVE_KEY" --acknowledge-new-uids restore
   ```

   Ordering is namespaces, Secrets, planes, environments, pipelines, types/
   traits/workflows/SecretReferences, projects, components, resources/workloads,
   other objects, releases, bindings. Unknown future kinds sort into the
   `other` group; review new dependencies before upgrading OpenChoreo. Restore
   defaults to create-only. An occupied OpenChoreo namespace requires --merge,
   including on reruns after partial success. Identical objects remain unchanged;
   each changed kind requires an explicit --allow-overwrite=Kind,OtherKind
   (including Secrets, *Binding and *Release). Live creation timestamps and
   managedFields timestamps newer than the archive prohibit updates even with
   that flag. Kubernetes has no universal last-modified timestamp: freeze all
   writers and retain managedFields; timestamps cannot prove absence of changes
   when that evidence was removed. Policy is preflighted before any writes.
   Updates use resourceVersion preconditions, preserve target status and
   unrelated metadata, never deletes target objects and can be rerun after a
   partial failure. Conflicts/admission failures stop execution; resolve the
   cause and rerun. Matching desired objects are unchanged on subsequent runs.
6. **Reconcile before reconnecting (180–210 minutes).** Compare desired releases,
   bindings and before snapshots with live data-plane inventory and audit
   records after the backup. Resolve changes newer than the archive through
   approved ChangeRequests, never blindly roll back prod. Map old and new UIDs,
   and prove generated selectors/resources are safe. This reconciliation is
   manual and is a release gate, not implemented automation. If it cannot be
   proven safe, stop here and use validated UID-preserving recovery; RTO is then
   missed, not silently declared achieved.
7. **Reconnect and validate (210–240 minutes).** Give the AEP operator the new
   aep-cp connection only after approving reconciliation. Its organization
   reconciliation recreates DataPlane registration using the long-lived
   per-cluster agent CA in management Secrets; preserve Organization UIDs and
   CA Secrets. Do not regenerate agent CAs or enable `generateCerts=true`.
   Reconcile gateway SANs, load-balancer/NodePort and inter-tenant CCNP paths:
   the stand needed a reflected gateway Service, and its target NodePort can
   change. Restore controller replicas and then the gateway under observation,
   reconnect agents, check DataPlane connectivity and application probes,
   compare selectors and active releases, and test one nonprod change. Verify
   protected prod writes remain denied. Re-enable operator/change/GitOps/backup
   schedules and incident deletion controls only after these checks.

Record actual timings, loss window, archive hash/version, approving people,
reconciliation results and workload availability. Run isolated recovery drills
quarterly, including corrupted archives, denied permissions, missing CRDs,
interrupted restore and a second idempotent restore. The four-hour budget above
is a target allocation, not measured evidence of meeting it.

## Not covered and acceptance blockers

- UID-preserving shared-etcd tenant recovery and automatic reconciliation of
  old/new UIDs or post-backup changes. Full scenario 8 remains unverified.
- Management cluster state: Organizations, ChangeRequests, CA keys, operator
  credentials, Cozystack objects and audit spool. Loss of management CA keys
  requires a separate controlled agent trust recovery, not this runbook.
- Runtime namespace resources, application databases/PVCs, data-plane clusters,
  OpenBao, Keycloak, portal Postgres, registries and external ITSM.
- OpenChoreo system-namespace Secrets/configuration, Helm release state, CRDs,
  admission policies and other non-OpenChoreo API groups. Reinstall from backed-up
  values, secrets management and Git before restore. Organization Secrets are
  included, but Kubernetes ServiceAccount tokens are intentionally excluded.
- Consistent transactional snapshots, automatic schema migration across releases,
  dependency inference for future kinds, automatic UID/owner-reference rewriting,
  archive signing and end-to-end stand tests. No cluster commands were run.

S3 signing uses `AWS_REGION` (default `us-east-1`); set it to the bucket region.
The configured region avoids requiring bucket-location discovery permissions.

## Encryption, retention and purge

Backup requires `--age-recipient=age1...` (chart value `ageRecipient`).
Generate the X25519 key with `age-keygen`, escrow the identity offline and give
only its public recipient to the CronJob. A fresh symmetric file key encrypts
each archive; age wraps that key for the recipient **before any S3 Put**.
Restore requires `--identity-file`; it authenticates/decrypts the complete
envelope and validates archive checksums before API writes. Plaintext legacy
archives are not accepted by the restore CLI. Keep historical identities through
their archives' recovery windows and test recovery after key rotation.

Recommend Object Lock **GOVERNANCE** for DR archives with a finite recovery
window. Keep bypass permission away from backup/restore identities and reserve
it for an independently approved purge role. Immutable retention preserves
recoverability but also retains deleted organization Secrets until expiry.
COMPLIANCE retention prevents early purge even by administrators and can conflict
with deletion obligations. Governance permits a controlled purge when required;
audit that action separately. Lifecycle rules must cover noncurrent versions.
Key destruction can make ciphertext unrecoverable, but a shared recipient affects
every archive encrypted to it, and copies of the private key defeat crypto-erasure.
Plan retention, key custody and emergency purge together.
