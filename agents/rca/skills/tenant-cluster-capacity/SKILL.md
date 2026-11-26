---
name: tenant-cluster-capacity
description: Separate tenant control-plane failures from worker and guest capacity pressure.
---
Fetch cluster_health for the affected tier and infra_changes around the incident.
Correlate TenantControlPlane conditions with KubernetesNodes and VMI conditions.
Compare guest MemoryPressure, DiskPressure and PIDPressure if metrics exist.
Check quota_and_denials before recommending more workers: a namespace quota may
prevent new VMs or disks even when the autoscaler requests them. Do not equate a
running VMI with a healthy guest Node. Never query physical management Nodes.
State which layer failed and which observations exclude alternatives. If tenant
metrics are unavailable, identify the missing evidence rather than inventing it.
Draft an organization-change request with capacity, cost, rollback and checks.
