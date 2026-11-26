# Tenant Argo CD (stage 1)

Argo CD is an optional organization service in its **nonprod tier cluster**.
Flux remains the internal delivery mechanism. Nothing runs user repository code
on the management cluster or OpenChoreo control plane. Production deployments
continue through OpenChoreo promotion and change approval.

## Enable

Merge these fields into an existing Organization:

```yaml
spec:
  tiers:
    nonprod:
      argocd:
        enabled: true
        version: 9.7.1
  vcs:
    provider: gitlab
    url: https://git.example.org
    groupPath: engineering/acme
    credentialsSecretRef:
      name: acme-vcs
```

`version` is the chart version; omission selects the tested pin **9.7.1**
(application **v3.4.4**). Other versions are rejected until tested. Production
Argo CD is rejected by CRD and operator validation. Schema updates are included
in both `config/crd` and the operator chart; Helm does not upgrade existing CRDs.
The integration schema generator is `python3 tools/generate-organization-integrations.py`.
It preserves existing list semantics, uses `x-kubernetes-list-type`, and never
emits `uniqueItems`.

Set operator chart `argocdHostTemplate`, for example
`argocd.{{.Organization}}.nonprod.example.org` (binary flag
`--argocd-host-template`). Only `Organization` and `Tier` are template inputs.
Use a hostname allowed by the tier's Cozystack domain policy. DNS and TLS must
be provisioned for that hostname on the tier ingress; the chart uses nginx,
HTTPS upstream and its default TLS certificate. No new Gateway API or
cert-manager installation is attempted.

Configure the existing Keycloak URL and realm. In that realm, provision a
confidential client `aep-<organization>-argocd`, authorization-code flow,
redirect URI `https://<hostname>/auth/callback`, and a `groups` claim containing
flat group names (disable full group paths). Store its secret as `clientSecret`
in `argocd-<organization>-oidc` in the operator management namespace. Client
creation and the Keycloak groups mapper are administrator prerequisites; this
change does not assume permission to administer customer identity clients.
The operator copies this credential only to the restricted `argocd` namespace.
Local admin and exec are disabled. Dex and notifications are disabled;
ApplicationSet is scaled to zero.

## Authorization and delivery

Each OpenChoreo Project gets an `aep-<project>` AppProject and an exact
`gitops-<project>` namespace (long names receive a deterministic hash suffix).
The operator labels the namespace `aep.aenix.io/argocd=allowed` and enforces
restricted Pod Security. Existing `dp-*`, system namespaces and arbitrary
namespaces are never destinations. Developer groups can manage and sync apps
only within these AppProjects, read their projects and view logs. The
`aep-<organization>-platform` group administers the instance, with cluster
credential mutation explicitly denied. The default project grants nothing.

AppProjects allow only the configured corporate VCS group/org URL prefix,
including a trailing slash boundary. They deny all cluster-scoped objects,
RBAC, OpenChoreo and Argo resources. Kubernetes RoleBindings independently
restrict the controller to explicit workload kinds in its project namespaces;
there is no controller/server ClusterRoleBinding. NetworkPolicy writes are
possible only inside these namespaces. A validating admission policy prevents
Argo service accounts (including the server acting for instance administrators)
from editing AppProjects. Cluster credentials use an explicit namespace list.

The existing enforced baseline policy denies HTTPRoute, TLSRoute and GRPCRoute
attachments to `openchoreo-data-plane/gateway-default` outside `dp-*`. Argo's
HelmRelease depends on this baseline. The project workload namespaces can use
their own ingress/routes, subject to this admission boundary.

Delivery reuses the operator's management-side Flux HelmRelease contract:
`kubeConfig.secretRef.name=kubernetes-apps-admin-kubeconfig`, key
`super-admin.svc`, tenant-resource/sharding/target-cluster labels, and target
and storage namespace `argocd`. A pinned upstream HelmRepository supplies the
chart. This deliberately retains the existing delivery credential contract;
replacing the platform-wide remote installation credential is outside this
change. Runtime credential writes use the tier operator's namespace Role.

`ArgoCDReady` reports the HTTPS URL only after the release is Ready and runtime
credentials are written; `status.argocdURL` supplies the portal link. Project
and Secret changes are reconciled on the normal periodic reconciliation.
`VCSReady` is independent: missing workflow prerequisites do not prevent
Argo CD installation. Namespace contents are retained when projects disappear
or Argo CD is disabled; Helm removes the release/RBAC, not retained workload
namespaces. Disabling does not delete customer repositories, webhooks or
external credentials. Organization deletion retains the existing explicit
confirmation/decommission process.

## Verification and remaining deployment checks

Run `bash tools/check-argocd.sh`: it downloads the pinned upstream chart,
produces values from operator code, runs `helm lint` and `helm template`, and
checks every workload/init container for nonroot execution, dropped ALL
capabilities, RuntimeDefault seccomp, no privilege escalation and read-only
root filesystem. The allowed image inventory is also in
`.github/workflows/images.yml`. No new binary or wrapper chart is introduced.

Local tests do not validate a live Keycloak login, tenant ingress certificate,
Cozystack domain admission, ESO connectivity or real repository sync. These
remain deployment acceptance checks. The standalone Cozystack marketplace
ApplicationDefinition described in the design is not delivered here; this
stage exposes the service through Organization and the existing Flux path.

Upstream references: [chart 9.7.1](https://github.com/argoproj/argo-helm/releases/tag/argo-cd-9.7.1),
[declarative configuration](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/).
