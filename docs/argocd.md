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
    readCredentialsSecretRef:
      name: acme-vcs-read
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
Use a hostname allowed by the tier's Cozystack domain policy and point its DNS
at the platform ingress. With Argo CD enabled, the operator turns on the
nonprod tier cluster's `ingressNginx` addon in `Proxied` mode for exactly that
hostname, so the platform ingress passes TLS through to the tier ingress. It
also gives the nonprod node group the `ingress-nginx` role, because the platform
ingress only forwards to workers that carry it; enabling Argo CD on an existing
organization therefore rolls the nonprod workers once.
Set operator chart `argocdAcmeServer` (binary flag `--argocd-acme-server`) to an
ACME directory to have cert-manager in the tier cluster issue the certificate
over HTTP-01 (ClusterIssuer `aep-argocd-acme`); otherwise the tier ingress
serves its default certificate.

Configure the existing Keycloak URL and realm. In that realm, provision a
confidential client `aep-<organization>-argocd`, authorization-code flow,
redirect URI `https://<hostname>/auth/callback`, and a `groups` claim containing
flat names or full paths (`/aep-...`); both are mapped. Store its secret as `clientSecret`
in `argocd-<organization>-oidc` in the operator management namespace. Client
creation and the Keycloak groups mapper are administrator prerequisites; this
change does not assume permission to administer customer identity clients.
When Keycloak uses a private CA, add PEM `rootCA` to this OIDC Secret; the
operator includes it in Argo OIDC configuration. With Keycloak integration enabled,
the operator creates project developer groups as Projects appear. Realm group
administrators assign memberships; client creation and mapper administration remain
manual. Without integration enabled, administrators create these groups themselves.
The operator copies this credential only to the restricted `argocd` namespace.
Local admin and exec are disabled. Dex and notifications are disabled;
ApplicationSet is scaled to zero.

## Authorization and delivery

Each OpenChoreo Project gets an `aep-<project>` AppProject and an exact
`gitops-<project>` namespace (long names receive a deterministic hash suffix).
The operator labels the namespace `aep.aenix.io/argocd=allowed` and
`aep.aenix.io/organization=<organization UID>` for cost attribution, and enforces
restricted Pod Security. The first label selects the enforced Kyverno rule permitting
only ClusterIP Services without externalIPs (including headless Services). Existing `dp-*`, system namespaces and arbitrary
namespaces are never destinations. Each `aep-<organization>-<project>-developer` group can manage and sync apps,
read the project and view logs only in its own AppProject. The organization-wide
`aep-<organization>-developer` group is read-only across this tenant instance. The
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
HelmRelease depends on this baseline. The project workload namespaces publish through HTTPRoute, subject to this admission boundary.
The controller Role cannot create Ingress. Outside platform namespaces, baseline
admission reserves the Argo hostname and nginx snippet annotations, and denies
Service externalIPs cluster-wide to prevent CVE-2020-8554 traffic interception.

Delivery reuses the operator's management-side Flux HelmRelease contract:
`kubeConfig.secretRef.name=kubernetes-apps-admin-kubeconfig`, key
`super-admin.svc`, tenant-resource/sharding/target-cluster labels, and target
and storage namespace `argocd`. A pinned upstream HelmRepository supplies the
chart. Set `--argocd-chart-url` (chart `argocdChartURL`) to an HTTPS Helm mirror
for air-gap delivery; retain the pinned version. This deliberately retains the existing delivery credential contract;
replacing the platform-wide remote installation credential is outside this
change. Runtime credential writes use the tier operator's namespace Role.

`ArgoCDReady` reports the HTTPS URL only after the release is Ready and runtime
credentials are written; `status.argocdURL` supplies the portal link. Project
and Secret changes are reconciled on the normal periodic reconciliation.
`VCSReady` and `ArgoCDReady` are independent conditions and do not gate
Organization Phase. Missing WorkflowPlane is reported by VCSReady and an event;
Argo CD installation continues after VCS verification. Core provisioning and
policy failures retain their existing Phase gates. Namespace contents are retained when projects disappear
or Argo CD is disabled; Helm removes the release/RBAC, not retained workload
namespaces. Explicit `argocd.enabled: false` removes the operator-applied
`argocd/aep-oidc`, `argocd/aep-corporate-vcs`, HelmRelease, HelmRepository,
`vcs-<org>-build` and OpenChoreo `aep-corporate-vcs` SecretReference. Runtime Secrets
are deleted before uninstalling their credential Role. Removing spec.vcs deactivates Argo even if its enabled flag is retained and
removes the same managed credentials and delivery objects. Input Secrets, customer
repositories and remote webhooks are retained. An omitted argocd block supports
VCS-only builds; explicit disable suspends credential delivery until re-enabled. Organization deletion retains the existing explicit
confirmation/decommission process.


## Repository execution boundary

Applications in `argocd` must use HTTPS repository URLs without `..`, percent
encoding (including `%2e` and `%2E`) or `@`. Admission checks both source and
sources arrays. SSH, including `git@host:path`, is deliberately unsupported.
This keeps clone credentials and the AppProject origin boundary unambiguous.

The repo-server egress NetworkPolicy permits only the VCS hostname from spec.vcs,
Keycloak, cluster DNS and the namespace-local Redis cache on TCP 6379. The operator
resolves endpoint names into exact IPv4 /32 and IPv6 /128 peers with their HTTPS
ports on every reconciliation. DNS failure prevents publishing new values; there
is no unrestricted fallback. Standard NetworkPolicy cannot follow DNS changes
immediately: allow for Flux reconciliation delay, and use stable corporate
addresses or an egress proxy. A CNI FQDN policy can replace the IP peers when
operated by the platform team; preserve hostname, port and DNS restrictions.
Shared hosting IPs do not provide hostname-level isolation with standard
NetworkPolicy. DNS must use kube-dns-labelled pods in kube-system; NodeLocal DNS
installations require a platform-reviewed exact resolver peer. Policies are
additive: do not install another policy allowing broad repo-server egress.

Argo `configs.cm.helm.valuesFileSchemes` uses a whitespace sentinel (trimmed to an empty
list by Argo, retained by the Helm chart), disabling remote
HTTP/HTTPS values files; cmd params also set `reposerver.allow.oob.symlinks=false`.
Local values files inside the repository remain supported. The pinned upstream
version reads schemes from argocd-cm, not from a reposerver scheme cmd parameter.

## Verification and remaining deployment checks

Run `bash tools/check-argocd.sh`: it uses the cached pinned chart, downloading only
when absent, and
produces values from operator code, runs `helm lint` and `helm template`, and
checks every workload/init container for nonroot execution, dropped ALL
capabilities, RuntimeDefault seccomp, no privilege escalation and read-only
root filesystem. The allowed image inventory is also in
`.github/workflows/images.yml`. Add `quay.io` and `ecr-public.aws.com` to the
organization registry allow-list (or mirror these images and test the overrides).
Every enabled component, including Redis initialization, has CPU/memory requests
and limits. `TestRenderedArgoSecurity` renders the cached chart offline whenever
Helm and the chart are available, without requiring an environment variable.
No new binary or wrapper chart is introduced.

Local tests do not validate a live Keycloak login, tenant ingress certificate,
Cozystack domain admission, ESO connectivity or real repository sync. These
remain deployment acceptance checks. The standalone Cozystack marketplace
ApplicationDefinition described in the design is not delivered here; this
stage exposes the service through Organization and the existing Flux path.

Upstream references: [chart 9.7.1](https://github.com/argoproj/argo-helm/releases/tag/argo-cd-9.7.1),
[declarative configuration](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/).
