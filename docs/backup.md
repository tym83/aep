# Managed-service backup policies

Organization `spec.tiers.<tier>.services.backupPolicy` names an administrator-owned
ConfigMap in the operator namespace. The operator resolves it, creates a per-tier
cluster-scoped Cozystack BackupClass, and embeds the schedule/class in the bridge's
policy. A policy hash rolls the bridge Deployment when configuration changes.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: prod-daily
  namespace: aep-system
data:
  backupClassName: cozy-default
  schedule: '0 2 * * *'
  retentionPolicy: 30d
  # Required for Redis: set only after verifying bucket retention externally.
  objectStorageRetentionVerified: 30d
```

Reference `prod-daily` from the prod tier. The source BackupClass must cover the
allowed service kinds. The bridge reconciles a `backups.cozystack.io/v1alpha1 Plan`
for each application, checking claim UID ownership before updates:

```yaml
spec:
  applicationRef:
    apiGroup: apps.cozystack.io
    kind: Postgres
    name: example
  backupClassName: aep-acme-prod
  schedule:
    type: cron
    cron: '0 2 * * *'
```

## Retention and source contract

The inspected Cozystack `api/backups/v1alpha1/{plan,backupclass,restorejob}_types.go`
on `upstream/main` defines **no Plan retention field**. BackupClass is cluster scoped
and contains `spec.strategies[]` with application selector, strategyRef and optional
parameters. Do not put credentials into strategy parameters: the driver can copy
them to tenant-readable Backup status.

For Postgres, AEP clones the source `strategy.backups.cozystack.io/v1alpha1 CNPG`
strategy per tier and sets
`spec.template.barmanObjectStore.retentionPolicy` to the named policy's value.
That is the real Barman driver field used by Cozystack's default CNPG strategy.
AEP does not enable a second Postgres backup mechanism. The generated BackupClass
points its Postgres strategyRef to that tier's CNPG strategy.

Redis's default strategy does not provide the same Barman retention setting.
Configure lifecycle expiration and Object Lock on the backing object store and
verify them before setting `objectStorageRetentionVerified` equal to the requested
retention. The operator rejects an unverified Redis policy. This acknowledgment
does not configure or independently inspect S3 retention. Backup catalog retention
must be managed consistently by backup operations; AEP does not delete Backup
objects as a retention substitute. Driver-specific catalog pruning and isolated
credentials remain upstream/operations work.

Removing a policy reference does **not** delete existing Plans or backups. Changing
or disabling production protection needs the approved organization-change process;
this implementation does not invent an approval bypass. Plans are retained when
applications disappear; backup operators must explicitly retire obsolete schedules.
Per-tier BackupClasses and CNPG strategies are deliberately retained for historical
backup restoration. Archive and verify the backup catalog before tenant deletion.

## Monthly production restore verification

Enable operator `--restore-verification` (Helm `restoreVerification: true`); the
flag defaults to false. The leader-elected loop runs every minute, independently
of organization provisioning readiness. It creates one report per source application
UID and UTC month, for bridge-managed Postgres/Redis in prod tiers with a policy.

The journal is a ConfigMap in the operator namespace labelled
`aep.aenix.io/restore-verification=true`. A run selects the latest Ready backup,
creates a separately named application in the **same prod tenant namespace**, waits
for readiness and creates a RestoreJob with `backupRef.name` and a typed local
`targetApplicationRef`. Cozystack's target reference has no namespace field; AEP
never restores production data into nonprod. Only workload sizing/version/database
settings are copied; external exposure and backup settings are not copied.

After RestoreJob `Succeeded` and current-generation application `Ready`, the
operator connects to the temporary application's Service ClusterIP/port. The
report records Passed/Failed and the evidence, then removes the RestoreJob before
deleting the temporary application with UID preconditions. Ownership labels and
namespace/target checks prevent adopting or deleting a live/foreign application.
Reports survive cleanup and operator restarts; unfinished reports from previous
months continue. A six-hour timeout marks failure and starts cleanup. Failure to
remove finalizers remains visible as Cleanup and is retried, never force-cleared.

This is a **TCP connection check**, not an authenticated SQL query, data checksum,
or proof of application-level data correctness. The report says so explicitly.
Supply an application-specific data verification extension before treating a drill
as a business recovery acceptance test. The operator needs network access to the
prod tenant's temporary Service ports and sufficient quota for a temporary copy;
no nonprod network opening is created. Test this path and the selected driver
versions on a staging installation before enabling monthly runs.

## Operational boundaries

Provision production backup storage with Object Lock in the dedicated backup
service tenant. Cozystack currently projects shared `cozy-backups-creds` into tenant
namespaces; do not claim credential or object-prefix isolation until that dependency
is addressed. Independent backup catalog export, Veeam certification, air-gap
validation and authenticated application data verification are outside this change.
No live restores or backup changes were performed by repository unit tests.

Tests cover Plan schema/claim ownership, CNPG retention cloning, same-namespace
restore success and cleanup, and rejection of foreign targets. The controller is
implemented, not a scheduled placeholder; a real cluster acceptance run remains
necessary to validate driver readiness, network policy and finalizer behavior.
