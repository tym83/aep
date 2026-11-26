# Cost ledger

The ledger implements hourly **provisioned-capacity chargeback**, with immutable
RateCard versions, PostgreSQL daily rollups, an organization-authorized read API,
a portal Cost page and Resource cost cards. It does not reconstruct historical
inventory from current objects or claim that provisioned charges measure utilization.

## Rate methodology for finance teams

1. Define an installation currency and a monthly cost pool: hardware amortization,
   support, power, facilities, storage, network and software subscriptions. Choose
   useful lives, residual values and expected billable capacity with finance.
2. Separate direct capacity pools from shared platform pools. Do not include the
   same management, aep-cp, OpenSearch or Harbor expense in both pools. Platform
   workloads outside organization tier namespaces are represented by these shared
   amounts, not by a second workload inventory charge.
3. Divide compute pools by billable vCPU-hours and GiB-hours. Price storage by
   class and GPU by the exact VM `deviceName`. Record discounts and capacity
   assumptions outside the RateCard under the organization's financial controls.
4. Select equal organization allocation or resource share for each shared pool.
   Resource share means direct rated provisioned cost divided by the installation's
   direct rated cost. With no direct demand it falls back to equal allocation.
5. Publish a new RateCard effective at a UTC hour boundary. Rates are nonnegative,
   and the entire spec is immutable. Duplicate effective dates stop collection.
   Keep old versions for audit and choose one currency per installation; the API
   groups by currency and never adds unlike currencies.
6. Reconcile monthly charges against the installation pool, review idle capacity
   and missing samples, then approve chargeback. Do not treat this internal dataset
   as a vendor invoice or evidence that invoices were independently reconciled.

```yaml
apiVersion: aep.aenix.io/v1alpha1
kind: RateCard
metadata:
  name: rates-2026-12
spec:
  effectiveFrom: '2026-12-01T00:00:00Z'
  currency: USD
  vcpuHour: 0.02
  memoryGiBHour: 0.003
  storageGiBMonth:
    replicated: 0.08
    local: 0.04
  gpuHour:
    nvidia.com/GA100_A100_PCIE_40GB: 1.5
  externalIPMonth: 2
  bucketGiBMonth: 0.02
  shared:
    management: {monthly: 500, allocationKey: resourceShare}
    aep-cp: {monthly: 300, allocationKey: equal}
    opensearch: {monthly: 100, allocationKey: resourceShare}
    harbor: {monthly: 100, allocationKey: equal}
```

## Accounting boundaries

The collector reads each Organization and its `tenant-<org>-<tier>` namespaces.
VM CPU and memory come from the referenced namespaced/cluster instancetype's
`spec.cpu.guest` and `spec.memory.guest`, or inline VM domain fields. It never uses
virt-launcher requests, which KubeVirt overcommit understates. Provisioned VMs are
charged even when stopped; their capacity is treated as reserved. Live instancetype
objects must be retained and not redefined in place. GPU devices are counted from
`spec.template.spec.domain.devices.gpus[].deviceName`.

Other nonterminal pods use scheduling requests (regular containers, init container
maxima, restartable sidecars, pod-level resources and overhead). PVC requested size
is charged once at its storage-class rate; VM disks are not also charged from pod
volumes. Monthly rates use the actual number of UTC hours in the calendar month.
Assigned load-balancer IPs are counted once per namespace. The collector reads
Postgres, Redis, Kubernetes and Bucket applications, matching Helm instance/release
and CNPG/Redis workload labels to their `aep.aenix.io/*` labels. Missing application
identity stays on the tier as unallocated, rather than being assigned by a name
substring. New managed kinds need an explicit inventory/identity adapter.

Bucket usage requires a trusted storage metering integration to refresh the app's
`aep.aenix.io/bucket-used-bytes` annotation (a Kubernetes quantity, e.g. `1073741824`).
The ledger does not poll object contents, substitute quota for actual usage, or
invent a value when metering is absent. A bucket without a measurement, an unknown
storage/GPU rate, or an inventory read failure rejects the installation snapshot.
Fractional HAMi accounting is not implemented; this stage accounts VM GPU devices.

`--tier-opencost-url-template=http://opencost.{org}-{tier}.example:9003` enables the
[OpenCost allocation API](https://opencost.io/docs/integrations/api/). The collector
queries the completed hour by pod, retaining `openchoreo.dev/project`/`component`
(or UID variants) and AEP labels, including Prometheus-sanitized label keys.
Weights are time-adjusted requested CPU and memory valued at the RateCard. Demand
below VM provisioned value leaves an `idle` row; excess demand is normalized.
This redistributes existing VM charges, never adds OpenCost monetary totals.
Unavailable, malformed or empty allocation responses keep VM charges on tier rows
with `unallocated=true`. PVC and managed-service charges remain separate.
OpenCost must expose a trusted, network-restricted endpoint for each tier cluster.

## Deployment and storage

Install the operator CRDs, create the RateCard, provision a Cozystack managed
Postgres and an S3 bucket, then install `charts/aep-cost-ledger`. For example:

```yaml
apiVersion: apps.cozystack.io/v1alpha1
kind: Postgres
metadata:
  name: cost-ledger
  namespace: tenant-aep
spec:
  version: v17
  replicas: 2
  resourcesPreset: t1.small
  size: 20Gi
  users:
    ledger: {}
  databases:
    ledger:
      roles:
        admin: [ledger]
```

Provision an existing Secret in the ledger namespace containing `COST_DSN`,
`S3_ENDPOINT` (host[:port]), `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`. Pass its
name as `credentialsSecret`; credentials do not need Kubernetes Secret read RBAC.
Use TLS and appropriately scoped database/S3 credentials. Flags override the
corresponding environment variables; S3 region and TLS are chart values. S3 is
optional when no endpoint/bucket is supplied. A configured bucket must exist.

`cmd/cost-ledger` samples at startup and hourly boundaries, using the current
inventory as an end-of-hour estimate for the **last completed UTC hour**. Restarts
retry the same hour without overwriting its first committed snapshot. Failed
attempts retry every minute. Missing hours during downtime are not backfilled;
short-lived workloads between samples are not observed. A PostgreSQL transaction
and advisory lock serialize the immutable hourly snapshot and daily rollup.
Embedded SQL migrations create `cost_hours`, `cost_days` and `cost_exports`.
Daily rows are retained indefinitely, including sampled-hour coverage. Budget
capacity for hourly audit snapshots too; no automatic retention deletion occurs.

Use one database per installation. For the read API, create a SELECT-only database
role with access to `cost_days` and configure `--cost-dsn`/`COST_DSN` (chart
`costCredentialsSecret`). The ledger writer owns migrations. Back up the ledger
Postgres using the same Cozystack backup mechanism as other managed databases.
Kubernetes access is read-only; the ClusterRole is cluster-wide because it serves
all organizations, while collection requests are limited to their tier namespaces.

`GET /v1/organizations/{org}/costs?from=2026-11-01&to=2026-12-01&groupBy=project`
accepts `project`, `service` or `tier`; `to` is exclusive, ranges are limited to
366 days. Developer, platform, admin, auditor and finance groups of that org may
read. Finance group: `aep-<org>-finance`; it has no change approval/write grants.
The backend retains the user's bearer token and forwards only supported queries.
Service grouping includes project identity to prevent collisions in Resource cards.

## Export and verification

Completed days are exported to `focus/YYYY/MM/DD.csv` and marked only after a
successful S3 PUT. Retries use the same key. Partial days remain explicit through
`x_SampledHours`; absent hours are not represented as zero consumption. CSV follows
[FOCUS 1.0 columns](https://focus.finops.org/docs/specification/v1-0/columns/), including
nullable conditional dimensions, JSON Tags, currency, billing/charge periods and
resource identity. Prices are internal chargeback prices: BilledCost, EffectiveCost,
ContractedCost and ListCost are equal, without discounts or amortization. Pricing
quantity is one daily resource charge, with its computed daily price. This is a
FOCUS-shaped internal billing dataset, not a claim of third-party certification.
CSV only; no Parquet dependency is included.

Tests use an independent installation fixture with VM overcommit, GPU, managed
Postgres, disk, IP, bucket and both allocation keys. Organization sums reconcile
within 2% (and to floating-point precision), including OpenCost splitting. Tests
also validate FOCUS required columns, unknown-rate behavior, request accounting,
OIDC organization boundaries and backup safety. To exercise PostgreSQL transactions:

```sh
COST_TEST_DSN='postgres://user:password@localhost/test?sslmode=disable' go test ./internal/cost
```

The integration test truncates ledger tables: use an isolated test database.
Production acceptance still requires measuring collection completeness, testing
real S3 credentials and comparing a full month's chargeback to the finance pool.
