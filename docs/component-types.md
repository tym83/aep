# AEP component types

The platform team installs `charts/aep-component-types` **once on aep-cp**, after
OpenChoreo CRDs are installed. These cluster-scoped types are shared by all
organizations and are owned by the platform team, not organization developers.

```sh
helm upgrade --install aep-component-types ./charts/aep-component-types \
  --kube-context aep-cp --namespace openchoreo-control-plane
```

| ClusterComponentType | Component reference | Purpose |
| --- | --- | --- |
| `aep-service` | `deployment/aep-service` | Deployment with at least one endpoint, Service and HTTPRoutes |
| `aep-worker` | `deployment/aep-worker` | Deployment without endpoints |
| `aep-cron` | `cronjob/aep-cron` | Scheduled CronJob without endpoints |
| `aep-migration` | `job/aep-migration` | Run-to-completion schema migration Job without endpoints |

Developers select a type in their Component:

```yaml
spec:
  componentType:
    kind: ClusterComponentType
    name: deployment/aep-worker
```

AEP application clusters enforce Kyverno `aep-restricted` in all environments.
These templates run as non-root (UID 65532), use RuntimeDefault seccomp, drop ALL
capabilities, disallow privilege escalation and disable service account token
mounting. The root filesystem is read-only by default; an `emptyDir` is mounted
at `/tmp` (reserve the volume name `aep-tmp` and that mount path).
`environmentConfigs.runAsUser` is an optional positive integer, default 65532;
`environmentConfigs.readOnlyRootFilesystem` may be false for images that need it.
Images must support the selected UID. Existing configuration, dependency,
ExternalSecret and upstream allowed-trait handling is preserved; traits must
also comply with admission policies.

CPU/memory requests default to `100m`/`128Mi`, limits to `500m`/`256Mi`.
Override defaults through chart `resources.requests` and `resources.limits`;
per-environment values use `environmentConfigs.resources`. Limits are always
rendered, with schema defaults for omitted fields. Set cron schedules through
`environmentConfigs.schedule` (default `1 * * * *`). Job parameters include
`activeDeadlineSeconds` (300), `backoffLimit` (3), and `restartPolicy` (`Never`
for migration, `OnFailure` for cron).

The chart's `types.service`, `types.worker`, `types.cron`, `types.migration`
flags select which types to install. `allowedWorkflows` configures upstream
ClusterWorkflow names; an empty list disables build workflows. This chart does
not install workflows or traits. Existing release tooling automatically packages,
inventories and publishes the chart alongside other `charts/*` charts.

The upstream types remain installed, but their unmodified pod templates are
rejected on AEP application clusters. The operator/platform team may remove
them after migrating references. Registry allow-lists, signed digests in prod,
and the ban on `:latest` still apply; these types do not replace image policies.

## Migration ordering

`aep-migration` is a plain Job, not a pre-rollout hook. The supported
[ComponentType contract](https://openchoreo.dev/docs/reference/api/platform/componenttype/)
and [deployment pipeline](https://openchoreo.dev/docs/developer-guide/deploying-applications/overview/)
do not establish a migration-before-service completion gate between components.
An external release workflow must deploy the migration, wait for Job completion,
and only then roll out the service; do not rely on resource creation order or
`autoDeploy` to provide this guarantee. Use idempotent migrations. Kubernetes Job
pod templates are immutable: a new migration run requires a fresh Job identity
(or explicit lifecycle management); changing the image on an existing Job is
not a rerun mechanism. This chart does not automatically delete completed Jobs.
