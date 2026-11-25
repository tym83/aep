# Application cluster policies

The organization operator installs Kyverno and policy bundles through Flux remote
HelmReleases after `DataPlanesConnected`. `PoliciesReady` requires the current
Kyverno, baseline and tier bundle releases to be Ready in every physical tier.
The releases use exactly the data-plane delivery contract: the tenant namespace,
`kubernetes-apps-admin-kubeconfig`, key `super-admin.svc`, and labels
`cozystack.io/target-cluster-name=kubernetes-apps`,
`internal.cozystack.io/tenantresource=true`, `sharding.fluxcd.io/key=tenants`.
Organization ownership is the UID, never the organization name. The operator
removes upstream global namespace and group exemptions so only the exclusions
explicitly present in the policy bundles apply.

## Enforcement

| Bundle | Scope | Behavior |
| --- | --- | --- |
| `app-cluster-baseline` | Every tier | Forbid privileged containers, hostPath and host network/PID/IPC namespaces; require container CPU/memory limits; reject `:latest`; require organization UID on AEP namespaces. Audit by default, Enforce with `policy.mode: enforce`. |
| Baseline registry policy | Every tier | Exact registry hosts, optionally with ports; no paths or wildcards. Empty allow-list reports violations in Audit even when mode is enforce. Images must explicitly include an allowed registry host. |
| Baseline OpenChoreo isolation | Every tier, independent of mode | Enforce reserved `openchoreo.dev/*` labels on Pod metadata and Deployment metadata/templates outside `dp-*`. Only `system:serviceaccount:openchoreo-data-plane:cluster-agent-dataplane` is exempt. Enforce HTTPRoute/TLSRoute/GRPCRoute parent references to `openchoreo-data-plane/gateway-default` only from `dp-*`. |
| `app-cluster-nonprod` | Physical nonprod tier | Always enforce Kyverno `podSecurity`, level `restricted`, the latest Pod Security version supported by the pinned engine. Exclude only `kube-system`, `cozy-*`, `openchoreo-*`, `kyverno`, `aep-system`. |
| `app-cluster-prod` | Physical prod tier | Always Enforce: explicit `@sha256:` with 64 lowercase hex characters and cosign signature against the supplied public key. `mutateDigest: false`, `verifyDigest: true`, `required: true`. No keyless identity or transparency-log dependency. |

Pod rules include init/ephemeral containers and Kyverno-generated controller
rules. Ephemeral containers cannot declare resource limits; the baseline limits
rule therefore prevents adding them in enforce mode. Production image rules have
no namespace exclusions: mirrored infrastructure images must also be signed by
the trusted organization key and referenced by digest before subsequent upgrades.
The existing tier bootstrap happens before these policies are installed.
Background reporting does not evict existing noncompliant Pods.

The `small` profile has one physical nonprod cluster, including logical prod
environments; it receives the nonprod bundle and one admission replica. Use
`standard` for a separate production cluster and production image enforcement.
Production has three replicas of each Kyverno controller; nonprod has one.
`regulated` remains unsupported by the existing operator.

## Organization configuration

```yaml
spec:
  policy:
    mode: audit
    registries: [harbor.example.org, 'registry.example.org:5000']
    cosignPublicKey: |
      -----BEGIN PUBLIC KEY-----
      <organization cosign public key>
      -----END PUBLIC KEY-----
```

`cosignPublicKey` is an inline PEM **public** key. Never put the private signing
key in Organization. It is required before the production policies can become
Ready. Missing keys fail closed at `PoliciesReady`; they do not omit the image
policy. Public key rotation changes the production HelmRelease values. Sign
existing required images with the new key before switching. `mode` only changes
the configurable baseline rules; it cannot disable nonprod restricted security,
OpenChoreo isolation, or production signature/digest enforcement.

Cost labels are practical ownership checks, not telemetry identity: namespaces
named `aep-system`/`openchoreo-data-plane`, marked
`app.kubernetes.io/managed-by=aep`, or created by the tier `aep-operator` SA must
carry `aep.aenix.io/organization=<Organization UID>`. Bootstrap already sets it.
Arbitrary user namespaces and OpenChoreo-created `dp-*` namespaces are not
claimed by AEP merely because they exist. Namespace creation through a future
AEP provisioner must use this marker or the operator SA. No Pod label is trusted
as cross-organization telemetry identity; the ingest gateway must still derive
organization/account/index from the sending cluster's certificate (design 5.12).
The route policy complements Gateway `allowedRoutes`; it does not configure the
separate Argo CD Gateway or replace network isolation.

## Versioned delivery and air gaps

Pinned upstream: [Kyverno chart 3.9.2 / Kyverno 1.19.2](https://github.com/kyverno/kyverno/blob/release-1.19/charts/kyverno/Chart.yaml).
It supports the requested `kyverno.io` ClusterPolicy rules. Do not upgrade to a
version that removes legacy policy execution without migrating and testing them.
The bundle chart version is `0.1.0`.

```sh
make policies-package
# Publication is a release-engineering action; development does not push artifacts.
for bundle in baseline nonprod prod; do
  helm push dist/policies/app-cluster-$bundle-0.1.0.tgz oci://REGISTRY/aep/policies
done
```

Publish these three charts before deploying the operator revision. Configure
`charts/aep-operator` values `policyBundleRepository` (default
`oci://ghcr.io/tym83/aep/policies`) and `kyvernoChartURL` (default
`oci://ghcr.io/kyverno/charts/kyverno`) to the release registry or air-gap mirror.
The corresponding flags are `--policy-bundle-repository` and
`--kyverno-chart-url`. Mirror the chart and every referenced image as part of the
installation inventory; no keyless Fulcio/Rekor connection is used for image
verification. Private workload registries still require Kyverno pull credentials
and trusted registry CA configuration in the installation. The current operator
does not provision registry credentials or a signing service.

## Time-bound exceptions

Enable only namespaced PolicyExceptions in `aep-system`. A native
ValidatingAdmissionPolicy guards both `kyverno.io` and `policies.kyverno.io`
PolicyExceptions, so an exception cannot exempt itself from this guard. Only
`system:serviceaccount:aep-system:policy-exception-manager` may create/update
exceptions. It requires both annotations:

```yaml
metadata:
  namespace: aep-system
  annotations:
    aep.aenix.io/change-request: acme/chg-approved-id
    aep.aenix.io/expires: '2026-12-01T12:00:00Z'
```

The guard validates RFC3339 syntax and timestamp parsing. The operator's separate
leader-elected maintenance loop deletes expired exceptions each minute, with UID
and resourceVersion preconditions to protect concurrent extensions. It does not
need admin credentials: the baseline chart grants the tier-local operator SA
only report listing and exception listing/deletion in `aep-system`. Existing
bootstrap credentials acquire these extra permissions when the chart installs.
Cleanup runs independently of provisioning gates; transient errors are retried
on the next tick. Exceptions may remain effective until cleanup succeeds; monitor
operator errors, especially during management/tier API outages. Invalid legacy
expiry annotations are left for administrator review rather than guessed.

No user-facing admin role receives exception-writing rights from these charts;
Kyverno admin-role aggregation is disabled. Provision and bind the dedicated
executor only to a trusted approval integration. The existing change service
still has no `policy-exception` execution handler: the annotation is an audit
reference, not proof that a ChangeRequest was approved. Until that integration
exists, a platform-controlled executor must verify approval before creating an
exception. Do not delegate its token to organization users. Cluster-admins remain
part of the platform trust boundary and can change admission configuration.

## Compliance snapshots

A leader-elected operator maintenance loop reads `wgpolicyk8s.io/v1alpha2`
PolicyReport and ClusterPolicyReport objects in each application cluster every
five minutes. It stores `compliance-<org>-<tier>` ConfigMaps in the operator
namespace (normally `aep-system`), with the Organization UID ownership label.
The JSON contains collection time, per-policy pass/fail/warn/error/skip result
counts and up to 50 unique failing policy/rule/resource tuples, ordered by policy
and resource identity. It supports both report-level `scope` and legacy
result-level `resources`. It excludes report messages, Pod specs and credentials.
Counts are per result, not unique resource counts. Large policy sets exceeding
the ConfigMap budget fail collection instead of silently truncating counts.

A collection is published only if both report lists succeed; failed collection
preserves the previous snapshot and timestamp. Missing snapshots mean unknown,
not compliant. The read API labels snapshots older than six minutes stale. Empty
reports mean no observed results, not proof that policies ran. Admission-only
rules may not produce background results. Deletion removes owned snapshots as
part of the existing ordered organization cleanup.

Build `Dockerfile.read-api` and enable `readApi` in the operator chart, supplying
its image, OIDC issuer and required audience. Set portal `aep.readApiUrl` to
`http://aep-read-api.aep-system.svc:8080`. The service supports `/v1/me` and
`/v1/organizations/{org}/compliance`, with verified OIDC and organization-specific
developer/platform/admin/auditor access. It has no tier kubeconfigs or Secret
permissions. Existing services/changes read endpoints remain separate integration
contracts; route them through an existing read gateway if already implemented.
See [portal API](portal-api.md) for the response contract.

## Validation

```sh
make policies-render
make policies-test # requires Kyverno CLI 1.19.2 locally
make policies-package
go build ./... && go vet ./... && go test ./...
cd portal
npx --yes @yarnpkg/cli-dist@4.13.0 tsc
npx --yes @yarnpkg/cli-dist@4.13.0 test --watch=false --runInBand plugins/aep plugins/aep-backend
```

The npm package `yarn@4` is not published; `@yarnpkg/cli-dist` is the official
Yarn 4 distribution. Policy test resources and expected results live under each
bundle's `tests/`; `make policies-render` creates ignored rendered policy files.
The offline prod tests cover digest validation; signature verification requires
a reachable test registry with signed, unsigned and wrong-key digest images.
Before rollout, run live admission tests for agent identity, reserved routes,
PolicyException guard/expiry, signature failures, Flux remote installation and
cross-organization access. Helm rendering and Go unit tests cannot prove webhook
behavior on the target cluster.
