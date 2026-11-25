# Aenix Enterprise Platform — architecture

This document describes what AEP is made of, how the parts fit together and why. It is the public summary of a longer internal design that went through two rounds of role-based review (enterprise buyer, security, Cozystack core, OpenChoreo, SRE, controller engineering, ITSM, FinOps and data protection, developer experience, AI) and through validation on a live Cozystack 1.7.1 installation.

## Goal

A large organisation wants an internal platform that its developers use on their own and that its security, IT service management and finance functions accept. Cozystack already builds the infrastructure: tenants, managed databases and queues, tenant Kubernetes clusters, virtual machines, backups and GPUs. OpenChoreo already gives developers projects, components, environments, promotion and a Backstage-based portal. AEP connects them and adds what enterprises require on top:

- organisations with environments isolated by Cozystack tenants;
- managed services for applications through OpenChoreo resource types (the bridge);
- change management with segregation of duties and an ITSM-neutral contract;
- policy reports, audit export, cost allocation;
- AI-assisted incident analysis that sees the infrastructure below the application and runs on the customer's own GPUs.

## Topology

```
tenant-root
├── tenant-aep                     AEP service tenant: aep-cp cluster and its managed services
└── tenant-<org>                   organisation (parent tenant, no workloads)
    ├── tenant-<org>-nonprod       dev and stage: application cluster "apps", managed services
    └── tenant-<org>-prod          prod: application cluster "apps", managed services
```

- **Management cluster (Cozystack).** AEP controllers only: organisation operator, bridge instances, change service, ITSM adapters, audit and cost collectors. Nothing from OpenChoreo and nothing that executes user code runs here.
- **aep-cp.** A Cozystack tenant cluster that hosts the OpenChoreo control plane, the portal and the observability plane.
- **Application clusters.** A tenant cluster per environment tier of each organisation. Sibling Cozystack tenants cannot reach each other, so non-production workloads cannot reach production databases (verified on the stand).

## Components

### Organisation operator

A cluster-scoped `Organization` resource is the single declaration of a business unit. The operator turns it into tenants with quotas, tenant clusters, network paths, the OpenChoreo data plane in every cluster, the organisation namespace, environments, deployment pipeline and authorisation bindings in OpenChoreo, and bridge instances. Every step is idempotent and reports its own condition. Readiness of a tenant cluster is judged by the Kamaji control plane, the addon releases and registered nodes, not by the application `Ready` condition, which turns true before the control plane exists.

Only the operator writes platform objects in OpenChoreo (environments, data planes, pipelines, types). Organisation roles are explicit allow-lists, and writes to bindings in protected environments are reserved for the change service.

### Bridge

OpenChoreo models "this service needs a database" with resource types. A resource type renders an object into the OpenChoreo cell namespace on the application cluster; Cozystack managed services live in tenant namespaces on the management cluster. The bridge closes the gap:

1. An AEP `ClusterResourceType` (for example `cozystack-postgres`) renders a `ServiceClaim` into the cell namespace. Only the OpenChoreo agent may create claims (admission policy).
2. A bridge instance — one per organisation and tier, running on the management cluster with rights only in its tier tenant — validates the claim against an allow-list of parameters and limits, and creates the Cozystack application with a deterministic name.
3. When the application, its HelmRelease generation and the applied specification all agree, the claim becomes `Ready`; reasons and timestamps from Cozystack are copied into the claim, so OpenChoreo's SRE agent sees the state of the database below the service.
4. The bridge writes a connection Secret, a selector-less Service and an EndpointSlice pointing at the ClusterIP of the managed service into the cell namespace. Applications reach the database by a stable local name.

Production applications are never deleted by the bridge; deletion in production is a change request.

### Change service

Writes to protected environments go through `ChangeRequest`s: promote, rollback, resource update, restore, credential rotation, decommission and others. The service enforces segregation of duties (no self-approval, approver groups per organisation, a second person for emergency access), plan windows, one active request per target, staleness checks before applying, and applies changes through the OpenChoreo API with its own identity, so OpenChoreo authorisation and audit stay in the path.

### ITSM contract

AEP exposes a neutral contract instead of a ServiceNow-specific integration: CloudEvents out (CI upserts and retirements, change and alert events), REST commands in (catalogue, requests, decisions, notes) with idempotency keys and conditional updates, a CMDB snapshot with epochs for full reconciliation, and decisions confirmed by the human approver in AEP rather than asserted by an adapter. Adapters (ServiceNow first, then Jira Service Management and simpler ITSMs) translate the contract and connect outbound only.

### Fleet

Stage 1 manages one Cozystack installation and its tenant clusters, delivering everything through Flux HelmReleases with narrow kubeconfigs. External clusters (EKS, AKS, GKE, other distributions) join later through Open Cluster Management with an explicit trust level, using the SIG-Multicluster `ClusterProfile` API as the inventory contract. Several Cozystack installations follow the Cozystack Fleet API.

### AI layer

AEP does not build its own agent platform. It feeds OpenChoreo's agents with infrastructure context — first through the bridge status, later through a read-only Cozystack MCP server — runs agents per organisation behind an alert router, prefers a model served on the customer's GPUs, measures root-cause accuracy on a reproducible failure set, and applies agent suggestions only through change requests.

## Validation status

| Check | Result |
|---|---|
| Pods of a tenant cluster reach managed services of their own tenant; non-production cannot reach production | Passed |
| OpenChoreo 1.4 control plane and data plane run in Cozystack tenant clusters | Passed |
| Keycloak of Cozystack as the identity provider of OpenChoreo, roles from groups | Passed |
| Bridge with narrow rights: claim → Cozystack Postgres → connection in the cell namespace → OpenChoreo outputs → application connects | Passed |
| Agent certificates: per-cluster CA survives leaf reissue; agent cannot impersonate another plane | Passed |
| Cross-tenant agent path to aep-cp through an explicit network policy pair | Passed |
| Admission policies apply to the Cozystack aggregated API | Passed |

Upstream contributions found on the way: cozystack/cozystack#5442 (tenant egress to etcd and ingress at nesting depth two and more).
