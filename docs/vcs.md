# Corporate VCS integration

Stage 1 connects the customer's existing GitLab or GitHub/GitHub Enterprise.
An optional shared GitLab installation profile is described below. `gitea` is reserved in the API;
selecting it returns an explicit unsupported-provider error, never Ready.

## Organization contract

```yaml
spec:
  vcs:
    provider: gitlab
    url: https://git.example.org
    groupPath: engineering/acme
    ensureGroup: false
    credentialsSecretRef: {name: acme-vcs}
    readCredentialsSecretRef: {name: acme-vcs-read}
    caBundleSecretRef: {name: corporate-git-ca}
```

For GitHub use `provider: github`, `url: https://github.com` (or the Enterprise
HTTPS origin), and `org: acme` instead of `groupPath`. The URL is an origin,
without embedded credentials, query, fragment or application subpath. The
repository allow-list is `<origin>/<groupPath-or-org>/`, not a string prefix
that could match an attacker-controlled neighboring group. GitLab subpaths
in group names are supported; installations hosted below a URL path are not.

Secret references are names in the **operator's management namespace**. Every
referenced admin/read/CA Secret must carry `aep.aenix.io/organization=<org name>`;
missing or foreign labels are rejected. This input ownership contract uses the
name, while operator-created resources use the Organization UID.
`readCredentialsSecretRef` is required and must refer to a different Secret from
`credentialsSecretRef`. Grant its token clone/read permission only. The admin
token is used solely for verification, group administration and webhooks. A token
Secret contains `token` and optionally `username`; defaults are `oauth2` for
GitLab and `x-access-token` for GitHub. A GitHub App Secret instead contains
`githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey` (RSA PEM).
App assertions are exchanged by the operator for installation tokens. Read
credentials request only `contents:read`; Argo receives username/password with an
installation token, never App IDs or a private key. Both token classes are cached
separately until expiry minus five minutes and refreshed during reconciliation.
Use a dedicated read-only App installation or read-only token in the read Secret. A CA Secret contains
`ca.crt`; system roots remain trusted. TLS verification is never disabled,
redirects are refused, timeouts are bounded, and provider response bodies and
transport errors are not included in Organization conditions.

Use separate administrative and clone credentials limited to the organization. Reading repositories
requires Git content read access. GitLab group creation requires appropriate
group creation privileges; webhook reconciliation needs repository webhook
administration. GitHub Apps need repository Contents read and Webhooks write
when hooks are enabled. Token verification uses the authenticated user endpoint;
GitHub Apps verify credentials through the installation token exchange.

With `ensureGroup: true`, missing GitLab group/subgroup path segments are created
as private groups; existing groups are reused. Without it, the group must exist.
GitHub organizations must already exist: ordinary organization credentials
cannot create a corporate organization through this API. `ensureGroup` is
therefore rejected for GitHub/Gitea. Reconciliation never deletes remote groups.

## OpenChoreo credentials

The operator writes a dedicated `vcs-<org>-build` Secret with `username` and
`password` in its management namespace, and a **SecretReference only** named
`aep-corporate-vcs` in the OpenChoreo organization namespace. GitHub installation
tokens are refreshed on reconciliation. Do not grant developers access to
management Secrets, Argo CD Secrets, or workflow execution namespace Secrets.
The OpenChoreo SecretReference is protected by the operator admission policy.
Non-operator SecretReferences cannot read any `remoteRef.key` starting with
`vcs-`, including references with an otherwise ordinary name.

Provision a namespaced **WorkflowPlane `nonprod`** for the organization, with
`spec.secretStoreRef` pointing to an ESO Kubernetes ClusterSecretStore in that
workflow cluster. Configure that store to read only `vcs-<org>-build` in the
operator management namespace through a dedicated read-only service account.
This is a required infrastructure prerequisite: the operator does not create a
workflow plane or grant cross-cluster secret-store credentials. Without the
plane/store reference, `VCSReady=False/Pending` states exactly what is missing.
Argo CD can still reconcile independently. A configured reference is not proof
of ESO connectivity; verify ExternalSecret readiness before running builds.

The reference maps remote properties `username` and `password` to a
`kubernetes.io/basic-auth` Secret targeted at WorkflowPlane `nonprod`. Choose
`aep-corporate-vcs` in the workflow's Git authentication parameter. No token is
written into Organization specs/status, portal payloads, ConfigMaps or Helm
values. Argo receives `argocd/aep-corporate-vcs` with label
`argocd.argoproj.io/secret-type=repo-creds`; the credential URL uses the same
allow-list boundary. Corporate CA roots are delivered to Argo CD's TLS
ConfigMap. The workflow image must also trust corporate Git CA roots.

## Build webhooks

The inspected OpenChoreo source supports `POST /api/v1alpha1/autobuild`, GitHub
HMAC-SHA256 and GitLab shared-token verification. Its source contracts are:
`internal/openchoreo-api/services/autobuild`, `services/gitsecret`,
`api/v1alpha1/component_types.go` and `secretreference_types.go`.

Set operator `--vcs-webhook-url` / chart `vcsWebhookURL` to that externally
reachable HTTPS endpoint. An empty value explicitly disables automatic
registration. Provision `openchoreo-control-plane/git-webhook-secrets` on the
control plane with `github-secret` and/or `gitlab-secret`. OpenChoreo currently
uses a **shared secret per provider**, not per organization; the operator reads
this existing secret and never overwrites it.

For organization Components with `spec.autoBuild: true`, the operator follows
the referenced Workflow/ClusterWorkflow schema's
`x-openchoreo-component-parameter-repository-url` extension to locate the URL
in workflow parameters. Out-of-allow-list URLs are rejected. Existing hooks
are compared by target URL, push events, enabled state and TLS settings before
PUT/PATCH; absent hooks are created, and lists are paginated. Since providers do
not return webhook secrets, the first matching hook after operator restart is
updated once to establish the desired secret, then unchanged hooks are left alone.
Secret rotation triggers an update on the next hook pass. Hook reconciliation
runs at most once per 15 minutes per organization within an operator process;
restarts reset this interval. Token refresh and credential delivery continue on
the regular reconcile interval. Hooks register push events with TLS verification.
Release-line-specific repository overrides are not enumerated; those need
explicit hook configuration. Remote hooks are retained on disable/removal to
avoid deleting customer-managed integrations sharing the endpoint.

`VCSReady` covers credential verification, group lookup/creation, registration
of the reference, and configured webhook operations. It is separate from the
Organization Phase gate, as is ArgoCDReady. Missing WorkflowPlane is reported
without blocking other VCS work. Per-component errors emit VCSComponentSkipped
events and skip only that component. Errors never expose credential values.
Removing spec.vcs deletes `vcs-<org>-build` and its SecretReference. Explicit
Argo disable also clears operator-managed Argo Secrets, HelmRepository and build
credentials; see [cleanup semantics](argocd.md). Input Secrets and remote hooks
are retained. Use trusted platform-controlled build workflows.

## Portal and tests

The authenticated `/v1/me` response includes optional `argocdUrl` and
`repositoryUrl` for each organization the caller belongs to. The entity card
uses the entity's organization annotations, links to its source annotation
(`backstage.io/source-location` or `aep.aenix.io/repository-url`), and otherwise
falls back to the corporate group URL. Only credential-free HTTPS links render.

`go test ./internal/vcs` runs fake HTTPS servers for both providers: credentials,
group lookup/creation, idempotent hook updates, redacted authorization/rate-limit
errors, redirect rejection and CA verification. Follow [portal checks](portal.md)
for the UI. No tests contact a customer VCS or Kubernetes cluster.

Provider references: [GitLab authentication](https://docs.gitlab.com/api/rest/authentication/),
[GitHub App installation tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app).

## Optional installation profile

`charts/aep-gitlab` is a small operator-independent wrapper around the official
[GitLab chart](https://docs.gitlab.com/charts/), pinned to **10.6.2 / GitLab 19.6.2**, verified against the official chart repository and
[patch announcement](https://docs.gitlab.com/releases/patches/patch-release-gitlab-19-6-2-released/)
on 2026-11-25. Review later security releases before deployment and follow required
upgrade stops with a restore rehearsal. There is **one instance
per installation**, in `tenant-aep` on management or a dedicated tenant cluster.
Do not install this chart per Organization. An individual tenant's own GitLab is
an ordinary marketplace application, outside this profile.

The wrapper disables bundled Postgres, Redis, MinIO, ingress controller,
cert-manager, Prometheus, registry and runners. It uses Cozystack-managed Postgres,
Redis and S3 buckets. Gitaly repository storage remains a persistent volume and
must have its own capacity, backup and recovery plan; S3 does not replace Gitaly.

1. Provision Cozystack Postgres/Redis/Bucket applications in the installation
   service tenant using the schemas of the installed Cozystack release. Select
   HA sizes and backup policies, provision a dedicated database/user with GitLab's
   required extensions and Redis endpoint/authentication. Never reuse tenant DBs.
2. Provision separate buckets for LFS, artifacts, uploads, packages, backups and
   temporary restore data. Enable encryption, retention and lifecycle policies;
   grant only the required bucket operations to each credential.
3. On the GitLab target cluster, create referenced Secrets from managed service
   outputs via the installation's secret-management process. Use reachable TLS
   endpoints/CA bundles when GitLab is on a dedicated cluster; `.svc` addresses in
   default values illustrate management-local service names only. Configure
   Postgres `sslmode` and Redis TLS according to the chart's supported settings.
4. Prepare DNS, ingress class and TLS Secret; set `gitlab.global.hosts.domain` and
   `gitlab.global.ingress.class`. Supply exact endpoints and Secret names in a
   protected site values file. Install the release only after review.
5. Configure every participating Organization's `spec.vcs.provider: gitlab`,
   shared URL and distinct `groupPath`. Use Keycloak groups/explicit membership to
   separate organizations; shared login does not authorize repository access.

The referenced Secrets are:

| Secret | Key | Content |
|---|---|---|
| gitlab-postgres | password | Dedicated database user's password |
| gitlab-redis | password | Managed Redis password |
| gitlab-s3 | connection | Rails object-store connection YAML |
| gitlab-backup-s3 | config | Toolbox s3cmd configuration |
| gitlab-keycloak | provider | OpenID Connect provider YAML |
| gitlab-ingress-tls | tls.crt, tls.key | Public GitLab hostname TLS certificate |

Example object-store connection (create inside the Secret, not values):

```yaml
provider: AWS
region: us-east-1
aws_access_key_id: REPLACE_FROM_SECRET_MANAGER
aws_secret_access_key: REPLACE_FROM_SECRET_MANAGER
endpoint: https://s3.example.com
path_style: true
```

Register a confidential Keycloak OIDC client with redirect URI
`https://gitlab.example.com/users/auth/openid_connect/callback`. Provider Secret:

```yaml
name: openid_connect
label: Keycloak
args:
  name: openid_connect
  scope: [openid, profile, email]
  response_type: code
  issuer: https://identity.example.com/realms/aep
  discovery: true
  uid_field: sub
  pkce: true
  client_options:
    identifier: gitlab
    secret: REPLACE_FROM_SECRET_MANAGER
    redirect_uri: https://gitlab.example.com/users/auth/openid_connect/callback
```

Auto-created users remain blocked until explicitly admitted. Do not enable
email-based auto-linking without verifying the directory's email trust. Keep an
audited breakglass admin path for identity outages; restrict initial-root Secret
access and rotate it after setup. See [GitLab OmniAuth](https://docs.gitlab.com/charts/charts/globals/#omniauth)
and [external services](https://docs.gitlab.com/charts/advanced/external-db/).

Prepare and inspect without deploying:

```sh
helm dependency build charts/aep-gitlab
helm lint charts/aep-gitlab --set gitlab.global.ingress.class=nginx
helm template gitlab charts/aep-gitlab --namespace tenant-aep -f site-values.yaml \
  --post-renderer charts/aep-gitlab/post-render.sh
```

Retain Chart.lock; upstream packages are not committed. Validate all rendered
pods against restricted PSS for the selected site options, including hooks and
backup CronJobs. The upstream webservice Helm test Pod lacks a restricted security context, so
`helmTests` is disabled; perform external HTTP/repository smoke tests instead.
No privileged runners or Docker-in-Docker are part of this
profile. Images are pinned by the upstream chart/app version; mirror their exact
resolved references in disconnected environments.

## Operations and recovery

Monitor Gitaly latency/disk space, Sidekiq queues, web errors, Redis memory,
Postgres replication, object-store failures and backup age. Toolbox backup CronJob
runs daily; independently back up the database with Cozystack and archive GitLab
configuration/encryption Secrets. A recoverable backup requires a coordinated
GitLab application backup (repositories/database/metadata), object storage and
Secrets, not just database snapshots. Set RPO/RTO and test restoration to an
isolated target at the same GitLab version before upgrades. Do not declare recovery
successful until repository clone/push, LFS/artifacts, SSO and a CI publish work.

Follow [required upgrade stops](https://docs.gitlab.com/update/upgrade_paths/),
wait for background migrations at each stop and take verified backups. A Helm
rollback alone is not a database downgrade strategy. This change performs no
installation and no live GitLab integration test.

## Cozystack service contract

Provision PostgreSQL **16 or newer**; `postgresMajorVersion` is a required deployment
assertion checked by the wrapper (it cannot inspect a remote server during Helm
rendering). For a Cozystack Postgres application named `gitlab`, the chart release
is `postgres-gitlab` and the writable endpoint is
`postgres-gitlab-rw.tenant-aep.svc`. Use the CNPG writable service, not the cluster
name. The wrapper requires `sslMode: verify-full`; a narrow template override adds
hostname verification to upstream's verify-ca configuration. Supply
`gitlab-postgres-tls` containing `ca.crt`, `tls.crt`, `tls.key` for the dedicated
GitLab database role. The server certificate must cover the configured host.

For Redis application `gitlab`, the release is `redis-gitlab`, Sentinel service is
`rfs-redis-gitlab.tenant-aep.svc:26379`, and the monitored master set is `mymaster`.
`global.redis.host` is that master-set name when Sentinels are configured; do not
use a single Redis Pod or service as a master-discovery substitute. Enable TLS when
creating the Cozystack Redis service, use `rediss` and TLS on each Sentinel, and
provide `gitlab-redis-ca` to the GitLab trusted certificate bundle. Redis auth uses
`gitlab-redis/password`; Sentinel auth is disabled in the Cozystack default and must
match the installed service if changed. For a separate GitLab cluster, provide
routable Sentinel and Redis advertised addresses, not inaccessible `.svc` names.

These names and CA contracts were checked in Cozystack `upstream/main` under
`packages/apps/redis/templates/{redisfailover,certmanager,tenant-projection}.yaml`
and `packages/apps/postgres/templates/{db,tenant-projection}.yaml`.
`global.ingress.class` is mandatory. The bundled ingress/Gateway controllers remain
disabled. All rendered GitLab images, including hooks and init containers, enter
the release inventory; use its exact digests when mirroring the installation.
Upgrades must follow the official [upgrade paths](https://docs.gitlab.com/update/upgrade_paths/),
including all required stops and completed background migrations, rather than
jumping directly from the old 18.6.0 baseline to 19.6.2.

Always use `--post-renderer charts/aep-gitlab/post-render.sh` for Helm template,
install and upgrade. Set `AEP_REGISTRY_MIRROR` for disconnected installations.
The renderer pins every container/init image to the reviewed release digest,
rewrites its registry and rejects images absent from the inventory. It uses the
installed `aep-release` binary, with a source-checkout fallback for development.
This is necessary because upstream GitLab subcharts use different image-value
contracts. Keep the post-renderer and release inventory version with the chart.
