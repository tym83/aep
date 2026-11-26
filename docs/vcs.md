# Corporate VCS integration

Stage 1 connects the customer's existing GitLab or GitHub/GitHub Enterprise.
It does not install GitLab per tenant or per platform. Bundled GitLab and
external cluster fleets are later stages. `gitea` is reserved in the API;
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
    caBundleSecretRef: {name: corporate-git-ca}
```

For GitHub use `provider: github`, `url: https://github.com` (or the Enterprise
HTTPS origin), and `org: acme` instead of `groupPath`. The URL is an origin,
without embedded credentials, query, fragment or application subpath. The
repository allow-list is `<origin>/<groupPath-or-org>/`, not a string prefix
that could match an attacker-controlled neighboring group. GitLab subpaths
in group names are supported; installations hosted below a URL path are not.

Secret references are names in the **operator's management namespace**. A token
Secret contains `token` and optionally `username`; defaults are `oauth2` for
GitLab and `x-access-token` for GitHub. A GitHub App Secret instead contains
`githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey` (RSA PEM).
App assertions are exchanged for installation tokens; Argo CD receives its
native App credential fields and refreshes its own tokens. A CA Secret contains
`ca.crt`; system roots remain trusted. TLS verification is never disabled,
redirects are refused, timeouts are bounded, and provider response bodies and
transport errors are not included in Organization conditions.

Use a dedicated credential limited to the organization. Reading repositories
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
are updated by target URL (including secret rotation), absent hooks are created,
and hook lists are paginated. Hooks register push events with TLS verification.
Release-line-specific repository overrides are not enumerated; those need
explicit hook configuration. Remote hooks are retained on disable/removal to
avoid deleting customer-managed integrations sharing the endpoint.

`VCSReady` covers credential verification, group lookup/creation, registration
of the reference, and configured webhook operations. Errors are retried without
exposing credential values. Separate corporate credentials for API management
and clone-only use are a future extension; limit current credentials to this
organization and use trusted platform-controlled build workflows.

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
