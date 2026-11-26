# Organization-scoped infrastructure RCA

The AI layer adds infrastructure evidence to the upstream OpenChoreo SRE agent.
It does not implement a new agent or apply remediation. Deploy one agent and one
report database per organization; proposed changes remain ChangeRequest drafts.
The management-cluster MCP and the control-plane alert router are separate binaries
with separate Kubernetes credentials. No changes to the organization operator are
required by this implementation; deployments and RBAC must be installed separately.

## MCP evidence service

`cmd/cozystack-mcp` serves Streamable HTTP at `/mcp`, using the official Go SDK
pinned in go.mod. OIDC verification reuses internal/authn and requires issuer,
signature, expiration and audience validation. The verified `client_id` claim is
also extracted; an unverified claim or an arbitrary `azp` is never accepted.

Use **Keycloak >= 26** for the service-account `client_id` claim. Check existing
client mappers after an upgrade: the older `clientId` spelling does not authorize
MCP access. See the [Keycloak migration guide](https://docs.redhat.com/en/documentation/red_hat_build_of_keycloak/26.0/pdf/migration_guide/Red_Hat_build_of_Keycloak-26.0-Migration_Guide-en-US.pdf).
For older versions, add a **Hardcoded claim** protocol mapper on the dedicated
`aep-<org>-sre-agent` client only: Token Claim Name `client_id`, Claim value
`aep-<org>-sre-agent`, Claim JSON Type `String`, Add to access token ON.
Do not attach this fixed organization identity to a realm-wide shared client scope.
Configure the MCP audience separately and verify a freshly issued client-credentials
JWT contains both the expected `client_id` and audience before deployment.

```sh
cozystack-mcp --issuer https://identity.example/realms/cozy --audience aep-mcp \
  --tls-cert /tls/tls.crt --tls-key /tls/tls.key --listen :8443 \
  --max-rows 100 --max-bytes 65536 --requests-per-minute 60 \
  --metrics-query-url-template 'https://metrics.example/{namespace}/api/v1/query'
```

Use --mgmt-kubeconfig (MCP) or --cp-kubeconfig (alert router) only for stand testing; production uses a dedicated ServiceAccount.
HTTP requires explicit --tls-termination-proxy and a trusted HTTPS ingress; the agent
token-proxy uses HTTPS upstream; only its pod-local loopback endpoint uses HTTP.
Internal CA bundles must be trusted.

Authorization resolves token groups against registered Organization objects. A
matching `aep-<org>-<role>` group (optionally prefixed with `/`) with role
`developer`, `platform`, `admin` or `auditor`, or exact
`client_id=aep-<org>-sre-agent` grants that organization. Zero or multiple matches
are refused, including conflicting group/client memberships and ambiguous name
prefixes. Configure clients with one organization's groups only. Every tool's org
argument must equal that scope. `finance`, `approver` and `breakglass` do not grant
RCA access. Organizations with names rejected by operator name validation are skipped;
`platform` and `security` are reserved. Tier names follow `operator.Tiers` (small:
nonprod; standard: nonprod and prod);
management namespaces are derived as `tenant-<org>-<tier>`, never supplied by callers.
Stateless transport prevents a session from carrying another token's identity.

| Method | Evidence |
| --- | --- |
| infra_changes(org, since, until) | Tier events, app generation/spec digest and bridge annotation observations, HelmRelease revision/config digest history; newest first |
| managed_service_state(org, service) | App and HelmRelease conditions with lastTransitionTime, bridge references, CNPG primary/instances/phase, latest app Backup, optional PVC utilization |
| cluster_health(org, tier) | Kamaji TenantControlPlane, KubernetesNodes, VMI conditions and phase, tier quotas, optional guest node pressure |
| quota_and_denials(org) | ResourceQuota used/hard and recent quota-related FailedCreate/Forbidden events |

`service` is `tier/kind/name`, for example `nonprod/Postgres/orders`. Since/until
are RFC3339, defaulting to the previous day; windows over seven days are rejected.
Only explicit app kinds are supported. Backend lists are bounded to 1,000 objects
per resource/namespace and tool work to twenty seconds. Truncated backend lists
mean ordering applies to the retrieved subset. Unavailable CRDs, permissions or
metrics appear in `unavailable`; they never imply a healthy resource. Output has
row and serialized JSON byte caps. These caps apply to evidence, excluding MCP
protocol framing. Rate limits are per SHA-256 token fingerprint and per process;
use a shared gateway limit when horizontally scaling. Rate/replay caches are bounded;
RCA evidence is read fresh rather than cached across incidents.

Never grant Secret or TenantSecret access. No code path reads them. Specs are hashed,
not returned; arbitrary metadata, connection strings, pod templates, physical node
names and backup artifact URIs are omitted. Known bridge annotations and selected
status fields are projected, then credential patterns and email addresses are
redacted. Sensitive free-text markers suppress the whole message, including
multiline/quoted values. Pattern filtering is defense in depth, not a guarantee
against all possible confidential text. Treat events as untrusted data.
Scheduled events, virt-launcher Pod events, VMI lifecycle events (including node
assignment) and Node events are omitted. Add repeatable `--redact-pattern` flags
for installation-specific host identifiers, for example
`--redact-pattern 'host-[0-9]+' --redact-pattern 'rack-[a-z]+'`.
Patterns scrub evidence strings and keys, including availability diagnostics;
invalid expressions prevent startup.

Limitations: HelmRelease history contains revisions/digests, not historical app
specs or annotations. App observations use the latest managedFields timestamp and
a current snapshot, explicitly marked historyAvailable=false; they do not claim a
complete diff. Condition transition times describe current conditions, not a
persistent transition ledger. Events expire. Durable history requires an audit
backend. Guest Node objects are not read from the management cluster: node pressure
uses trusted tenant metrics `kube_node_status_condition` with `tenant_namespace`.
The configured endpoint must isolate tenant data and inject that label at trusted
scrape/ingestion time. PVC queries use exact namespace and CNPG-labeled PVC names.
No arbitrary PromQL, endpoint, management Node or LINSTOR topology query is exposed.

Install a ClusterRole with **get/list organizations.aep.aenix.io only**, plus
RoleBindings in each allowed tier for get/list on the following explicit resources:

- core: events, resourcequotas, persistentvolumeclaims;
- apps.cozystack.io: postgreses, redises, kuberneteses, kubernetesnodeses,
  kafkas, rabbitmqs, mysqls, clickhouses (only installed kinds need grants);
- helm.toolkit.fluxcd.io: helmreleases;
- postgresql.cnpg.io: clusters;
- kamaji.clastix.io: tenantcontrolplanes;
- kubevirt.io: virtualmachineinstances;
- backups.cozystack.io: backups.

No write verbs, wildcard resources, cluster-wide node permissions or secret grants.
Resource versions must match the installed CRDs; absent kinds are explicit gaps.

## Alert router and observer authentication

`cmd/alert-router` accepts only POST /api/v1alpha1/rca-agent/analyze, with the upstream
payload: namespace, project, component, environment, optional release_line/meta,
and alert containing id, value, timestamp and rule. It reads the Namespace from the
OpenChoreo control-plane cluster. The namespace name identifies the Organization. Its
`aep.aenix.io/organization` label must equal that Organization's UID, with
`openchoreo.dev/control-plane=true`.
No namespace supplied merely through an alert metric label is trusted on its own.
The sender is a trusted observer; these checks do not authenticate end-user alerts.
The ClusterRole requires `get` on core `namespaces` and
`organizations.aep.aenix.io`; no tenant resource or Secret access.

The destination defaults to `http://sre-agent.aep-{org}.svc:8080`. Template substitution
uses a validated namespace name, and redirects are forbidden. Payloads and responses
are capped at 1 MiB; errors from Kubernetes do not expose backend details. Caller
Authorization/HMAC headers are never forwarded. The upstream analyze route currently
has no application authentication, so keep the agent behind the provided policy.

**mTLS:** set --client-ca /tls/observer-ca.pem --tls-cert /tls/tls.crt
--tls-key /tls/tls.key. Use a dedicated observer client CA: any valid client-auth
certificate issued by this CA is trusted. Chains, validity and client-auth EKU are
verified. Do not use a broad organization CA. The HTTP handshake requires the
certificate when HMAC is disabled; no forwarded certificate headers are trusted.

**HMAC:** set --hmac-secret-file /run/observer/hmac, containing at least 32 random
bytes represented as text (trailing whitespace is trimmed). The sender adds
X-AEP-Timestamp (Unix seconds) and X-AEP-Signature (lowercase hex HMAC-SHA256 over
`timestamp + "." + exact raw request body`). The acceptance window is five minutes
in either direction; a signature is accepted only once per process in ten minutes.
A sender retry must sign again with a new timestamp. All pods share the same secret,
but replay caches are local: use one replica or add replay protection at a shared
gateway. TLS is still required on untrusted links to protect alert contents.
If both methods are configured, either authenticates; an invalid presented client
certificate fails the TLS handshake. Never log keys, signatures or bearer tokens.

The inspected observer uses `alerting.rca.service.url` (environment
`RCA_SERVICE_URL`) as one base URL and appends the analyze path. `AI_RCA_ENABLED`
must be true. Set that URL to the router/proxy base URL, not the analyze path.
The observer's current httpClient.Post has no mTLS/HMAC configuration: deploy a
trusted observer-side egress proxy that supplies mTLS, or add sender-side signing.
HMAC is not enabled simply by pointing the unchanged observer at the router.
Restrict router ingress to observer pods and protect the namespace labels with
platform-only RBAC/admission. Alert namespace label verification alone does not
prevent compromised telemetry senders from forging another organization's scope;
trusted ingestion binding remains required by the platform design.

## Agent and inference charts

`charts/aep-sre-agent` uses the upstream observability chart's image repository and
configuration variable names. Its development default tag is latest-dev, matching
the inspected chart. **Set image.digest and tokenProxy.image.digest to tested
signed images for production**; rendering fails if either is empty unless
`dev: true` is explicitly set.
The selected image must include src/extensions. The assets under agents/rca are
mounted as both `EXTENSIONS_DIR/rca` and `EXTENSIONS_DIR/chat`; remediation is disabled.
The ConfigMap includes mcp.json, CONTEXT.md and three single-file skills. The standalone
mcp.json points at `http://127.0.0.1:8081/mcp`; the chart uses `tokenProxy.port`
for its loopback URL. The token-proxy sidecar forwards to the configured HTTPS
`mcpUrl`, replacing Authorization with a client-credentials access token.
When editing assets, synchronize their copies under charts/aep-sre-agent/files/rca.

Deploy in namespace `aep-<org>` with organization, secretName, llm.baseUrl,
llm.modelName, oauth.tokenUrl and jwt issuer/audience/jwksUrl configured. The existing
per-org Secret contains RCA_LLM_API_KEY, OAUTH_CLIENT_SECRET and
SQL_BACKEND_URI. This mounts credentials as environment values without granting the
pod Kubernetes API access. The proxy obtains tokens from `oauth.tokenUrl` using
OAUTH_CLIENT_ID and OAUTH_CLIENT_SECRET and caches them until 80% of expires_in.
Configure the client's audience for MCP; `oauth.scope` supplies optional scopes.
Token refresh is automatic; after rotating the client secret, roll out the Deployment
to reload its environment. caSecretName optionally supplies a
complete trusted CA bundle in ca.crt. Give the agent's OAuth client only organization
read permissions in OpenChoreo and observer; prompt instructions alone are not authz.

The default report backend is an external per-org PostgreSQL database. Provision it
separately, retain reports for 90 days, enforce cleanup and audit access. Retention,
external-LLM consent and model request auditing are operational integration work,
not controllers implemented here. Portal chat/report routes rely on upstream OIDC
and OpenChoreo authorization. NetworkPolicy allows port 8080 only from the configured
alert-router and portal pod selectors AND their namespace selectors; do not grant
untrusted users those labels. Policies are additive: do not install another policy
that opens ingress to these pods. Egress policy depends on the installation's API,
identity, database, DNS, MCP and inference endpoints and must be supplied there.

`charts/aep-llm` installs a pinned vLLM image, an OpenAI-compatible Service on port
8000, GPU requests/limits and a PVC mounted at /weights. Install inside the GPU tenant
cluster, not the management cluster. Set model explicitly; extraArgs can configure
the model's tool-call parser, automatic tool choice and tensor parallelism. Mirror
weights/images for disconnected installs. The v0.11.0 manifest digest is pinned in
values. Empty image.digest is rejected unless dev is true. apiKeySecret is required
unless `allowUnauthenticated: true` is explicitly set for an isolated stand.
The ingress NetworkPolicy defaults to pods labelled
`app.kubernetes.io/name=aep-sre-agent` in the same namespace, on TCP port 8000 only.
Set `networkPolicy.allowedSources` to explicit Kubernetes NetworkPolicy peers for
other namespaces; an empty list denies all ingress. Both pod and container run
nonroot with RuntimeDefault seccomp; the container drops all capabilities and
forbids privilege escalation. Preserve the PVC
on upgrades and plan retention before uninstalling the release. No model quality or
compatibility claim is made: validate tool calls and structured output on the stand
before accepting an inference backend. External inference needs explicit org consent.

## Evaluation and delivery status

See ../eval/README.md for thirty failure definitions, the runner, recovery and score
semantics. One real quota injector and one simulated disk injector are implemented;
28 automated injectors are TODO. Simulations/stubs cannot satisfy the 70% acceptance
gate. There is no claim of an end-to-end stand run or GPU inference validation.

The images workflow builds cozystack-mcp, token-proxy, alert-router and rca-eval alongside existing
images. Build/test/vet and Helm rendering validate the local implementation; deploy
with signed immutable images and review tier RoleBindings before a stand exercise.
Remaining integration gates: observer authentication proxy, installed CRD/metrics
compatibility, per-org client-secret rotation/report lifecycle, inference compatibility,
and the complete thirty-case real-failure evaluation.

