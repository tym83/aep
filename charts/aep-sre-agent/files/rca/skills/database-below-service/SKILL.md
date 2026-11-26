---
name: database-below-service
description: Trace an application database symptom to its managed database and storage.
---
Read Resource and ResourceReleaseBinding conditions and bridge references.
Identify the exact organization, tier and Postgres app; do not guess another tenant.
Fetch managed_service_state, then infra_changes around the first failing request.
Compare application symptoms with CNPG primary changes, instance readiness, phase,
condition transition times, release revisions, PVC utilization and last backup.
A failover is not necessarily a root cause: establish whether it preceded the
symptom. Distinguish full PVC, failed storage attachment, exhausted connections
and a bad release. Missing metrics or backup records must be reported explicitly.
If storage evidence points below the tenant, request platform-team investigation.
Return a ranked cause with timestamps and evidence. Propose a resource-update
ChangeRequest draft with backup checks, rollback and verification; never execute it.
