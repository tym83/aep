---
name: quota-exhaustion
description: Identify the exact tier quota and denied resource behind a failed reconciliation.
---
Fetch quota_and_denials and cluster_health for the affected tier. Compare used/hard
for pods, requests.cpu, requests.memory, PVC count, storage and object counts.
Correlate FailedCreate/Forbidden events with the requesting workload and the first
symptom. An authorization denial without quota evidence is not quota exhaustion.
Use infra_changes to distinguish a lowered quota from increased consumption.
Separate management tenant quotas from guest application namespace quotas.
Do not delete workloads or enlarge limits. Draft an organization-change request
with the exact quota dimension, requested headroom, cost and rollback conditions.
Verify that queued reconciliations recover and no neighboring tier is affected.
