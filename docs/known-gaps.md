# Known gaps

- **Bootstrap authorization cleanup is eventual.** The operator prunes chart-managed
  broad cluster bindings on Organization reconciliation and reports
  `ControlPlaneHardened` on every Organization. Helm upgrades can recreate those
  bindings until the next reconciliation. On a legacy aep-cp no Organizations
  means no cleanup; a managed ControlPlane hardens itself (`Hardened`) also
  without Organizations, and they report its result. Unlabelled,
  foreign-managed and AEP-labelled offenders require manual review.
  Report-only mode never deletes. Verify the condition after upgrades; configure
  distinct `aep-<org>-publisher` IdP clients separately. See
  [operator hardening and flags](operator.md#installation).

- **Bridge reads Secrets cluster-wide in the application cluster.** Before writing its connection Secret the bridge reads the object by name to refuse adopting foreign objects, so its ClusterRole carries `get` on all Secrets. The admission policy restricts writes only. Fix options: per-`dp-*` namespace Roles rendered by the operator, or a pure server-side apply flow with ownership checks done through field managers instead of a read. Found on the stand on 2026-11-25.

- **Admin kubeconfig withholding is an upstream dependency.** Cozystack must expose
  `withholdAdminKubeconfig` to trusted organization provisioning. The operator
  uses super-admin only for the app-cluster bootstrap and contract upgrades, then scoped SA
  tokens. Flux remote HelmReleases still use `super-admin.svc` as required by the
  current Cozystack delivery contract; hiding it from tenant users remains pending.
- **Agent CA private key resides in the application cluster.** The verified
  cert-manager CA issuer requires both certificate and key in `aep-agent-ca`.
  Each tier has its own CA, also stored on management. A later CSR signing flow
  must remove this residual risk; CA rollover is currently manual.
- **Federation mapping, default SecretStore provisioning and tenant user OIDC RBAC
  remain manual.** See `docs/operator.md` before deploying. Managed-service backups
  are covered by [backup policies](backup.md).
- **Kafka state is visible only through ServiceClaim status.** Strimzi runs in
  the tier application cluster, which the read API, portal and MCP evidence
  (management-cluster clients) cannot read; Kafka has no backup integration and
  external fleet clusters do not receive Strimzi.
- **Deletion requires a maintenance window.** Cross-cluster preflight cannot
  atomically freeze new Components/ServiceClaims. Stop writers before confirmation;
  an operator-reviewed decommission workflow is still needed. User cascading
  deletion is blocked by admission until that workflow exists.
- **Managed control plane limits.** Found while serving Organizations from a
  `ControlPlane` (not yet verified live):
  - External fleet clusters dial the single `--fleet-gateway-url` of the
    operator, not the gateway of their Organization's ControlPlane, and do not
    receive the CoreDNS entry for a fixed `spec.clusterGateway.host`: give that
    host a corporate DNS record and serve one control plane per fleet hub.
  - The fixed-host CoreDNS entry relies on the Cozystack coredns chart's
    `extraConfig` and its Corefile indentation (Cozystack v0.36 to v1.7
    inspected); a change there needs a corporate DNS record instead.
  - Build organizations are those with `spec.vcs`; the Organization API has no
    separate builds switch. `aep-builds` 0.3.0 takes one publisher client ID
    override for all organizations, so `publisherClientIDTemplate` must render
    its default or one shared ID.
  - Disabling content uninstalls it; a deselected ClusterResourceType is deleted
    even while Resources use it.
  - Whether Flux takes over content installed by hand with the Helm CLI under
    the same release names is unverified; adoption covers only
    ClusterResourceTypes.
  - `AuditReady` and backup readers (`spec.audit`, `spec.backup`) are not
    implemented.
