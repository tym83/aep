# Acceptance tests

[API harness and matrix](e2e/) · [Browser tests](ui/README.md) ·
[Moderated UX study and results template](ux/README.md) ·
[Manual CI workflow](../.github/workflows/e2e.yml)

## Running

Local validation never loads stand configuration by default:

```sh
go build ./...
go vet ./...
go test ./...
go vet -tags e2e ./...
go test -tags e2e ./tests/e2e
cd tests/ui && npm ci --ignore-scripts && npx tsc --noEmit
```

Copy [config.example.yaml](e2e/config.example.yaml) to a private file. Supply
valid component and application SecretReference specs for your installed types,
optional `resources.protectedSpecs` overrides for the three protected-object
creation payloads,
actual log/output routes and output field paths. For logs, set per-organization
`logsProject`/`logsComponent` to an existing running application in dev and prod
(the suite only reads it). Without both fields, logs cells skip with an explicit
reason. Routes and specs vary by OpenChoreo version. `secretSpec` must include
a `template` object and at least one `data` entry with `secretKey` and
`remoteRef.key`; configuration loading rejects missing fields. The example
uses an application key in ClusterSecretStore `default`, selected by the
nonprod DataPlane's `secretStoreRef.name: default`. `secretStores` belongs to
status and must not be placed in the spec. Supply two disposable
organizations with working release controllers, Postgres and cost ledgers.
Then explicitly run against that stand:

```sh
go test -v -count=1 -timeout=100m -tags e2e ./tests/e2e -config /private/path/config.yaml
```

The token helpers are argv arrays executed without a shell. OIDC and Backstage
helpers receive the username as their final argument and print only the token.
The cleanup helper receives an empty username and returns its dedicated platform
identity. Tokens are refreshed on each call (both OIDC and Backstage for the
portal). Transport failures retry once with the same body and idempotency key;
errors include only the URL path, never tokens, query strings or response bodies.
Never commit credentials, private endpoints or helper scripts. Each
role identity must have only its named organization role. The extra
`breakglass-requester` identity has developer + breakglass: the current create
API requires a creator role as well as the breakglass group. `security-oncall`
is a different person in `aep-security-oncall`. B needs developer and approver.
Users must be distinct, especially requester, approver and security on-call.

The suite creates unique projects/resources/components in both organizations,
waits for releases and binds nonprod. Configure component dependencies in
`componentSpec` using `{project}` and `{resource}`. Template variables also
include `{org}`, `{name}`, `{run}`, `{component}`, `{resourceType}`,
`{environment}`, `{prod}`, `{projectRelease}` and `{resourceRelease}`. The golden
path requires a current-generation Ready resource binding and all configured
connection outputs. Output values and HTTP response bodies are never logged;
redirects are refused. A timeout, 400, 5xx or missing Ready is a failure, not an
acceptable authorization result.

## Coverage

| DESIGN §0.1 scenario | Automated coverage in this suite |
|---|---|
| 1 Organization provisioning | Prerequisite; destructive provisioning acceptance belongs to operator tests, not this user suite |
| 2 Service with database | Developer project/component/Postgres creation, dev binding Ready, connection output presence; browser catalog and infrastructure card |
| 3 Promotion | Direct prod denial, request, fresh-version self-approval denial, independent approval, Applied and binding existence, rejection, approved decommission; browser approval |
| 4 Compliance | Authorized and forbidden API/portal reads; auditor page; exception execution is outside this brief |
| 5 Costs | Finance and other roles' API/portal permissions; finance-only operational denials; accounting reconciliation/export is outside this brief |
| 6 Backup/restore | Outside this brief; no backup or restore acceptance claim |
| 7 Audit | Unique justifications, retained ChangeRequests, notes and Recorded breakglass; no SIEM delivery claim |
| 8 Control-plane disaster recovery | Outside this brief; no destructive DR execution |
| 9 Offline installation | Outside this brief; no installation/upgrade execution |
| 10 Organization isolation | Every scoped API and portal matrix cell A→B and B-developer→A; body marker checks; browser catalog/direct entity; optional MCP, pod TCP and management kubectl checks |
| 11 Failure diagnosis | Outside this brief; MCP evidence authorization only, not diagnostic accuracy |

All requested item 3 role journeys are executable. Auditor and finance journeys
are their complete matrix rows; platform adds application-secret creation and
protected object denials; admin adds restricted role/binding creation and foreign
binding denial. Approval and breakglass use real asynchronous state transitions.
This suite does not claim all of DESIGN §12 stage 6 is accepted: that broader
stage includes destructive infrastructure scenarios beyond this task, and a
stand execution is required to establish actual acceptance.

## Matrix semantics and resolved expectations

[matrix.yaml](e2e/matrix.yaml) is JSON-form YAML, ordered and reviewable, with
nine roles and explicit actions, methods, payloads, success codes and allow
lists. Default denial is 403, anonymous 401; cross-org object reads and writes
accept 403/404. OpenChoreo list cells also accept 200 with no items belonging to
the target organization. All denied lists and cross-org responses check that neither organization nor fixture/private marker names occur in the
body. `/me` is identity-scoped: it returns 200 and must exclude the other org.
Each failing subtest prints a role/action/target/expected/actual row. There are
**no pending cells**. Portal equivalents cover every route the backend exposes;
notes have no portal proxy route. Decisions use fresh Pending decommission
records for absent run-owned targets, so allowed matrix approvals cannot create
production data. `approve-own` applies only to creator roles in their own org.
`approve-others` uses a different creator and allows the approver; other roles
expect denial. Portal cells follow the same rules. The admin journey copies
the operator binding's exact nonprod conditions for its namespaced role and
also checks that an unrestricted delegated binding is rejected with 422.

- Finance has no OpenChoreo role: object actions return 403, lists are filtered. Finance-only
  read API access is `/me` and costs; services, changes and compliance return 403.
- Developer/platform production logs return 403 under RenderAuthz conditions;
  their nonprod log reads succeed. Breakglass-only has no generated OpenChoreo
  role either.
- The change API deliberately has a broader member contract than read API:
  auditor, finance and breakglass may read individual audit records and add
  notes (201). They cannot create promotions or approve them. Thus "read-only"
  describes infrastructure writes, not audit annotations. Tests preserve this
  implemented contract instead of expecting every POST to fail.
- Breakglass must reach Applied with reason `Recorded`. The actual temporary
  grant, expiry, alert and credential rotation are not implemented by the product;
  this test makes no claim that production access was granted.
- MCP is skipped only when its endpoint is omitted. Its four tools use the
  Streamable HTTP protocol. Auth failures are HTTP 401/403; a permitted identity
  targeting another org receives an HTTP-success tool result with
  `isError=true`, `organization mismatch` and no target data. It cannot honestly
  be asserted as HTTP 403. Other protocol or tool failures fail the test.

## Cleanup and audit retention

OpenChoreo user roles cannot delete projects/components/resources by design.
Tests register cleanup before each create and delete only exact run-named
objects (also labeled `aep.aenix.io/e2e-run`). Nonprod cleanup uses the dedicated
`cleanup.tokenCommand` platform identity, or, when absent,
the Kubernetes Go client with `cleanup.aepcpKubeconfig` against **aep-cp**.
Discovery and namespace reads must succeed before setup; an unreachable API
fails fast with a diagnostic. No kubectl process is used for cleanup. It never
deletes a namespace, selects an entire organization, or touches existing resources.
Deletion is retried until absence is confirmed or the configured timeout expires.
A final sweep, registered before setup, finds run-owned objects including
controller-generated releases, then lists again and reports swept/remaining/error
counts. Kubeconfig mode discovers OpenChoreo resource kinds; token mode uses the
fixture kinds and follows list pagination. Cleanup errors or leftovers fail
the run. Objects with a production binding are retained if approved decommission cannot confirm its removal.

Production resource bindings are removed exclusively by a `decommission`
ChangeRequest with `desired.deleteData: true`, created by the developer,
approved by a separate approver, observed Applied and verified absent.
Applied/Finalizing alone is insufficient. This also handles a regression that
unexpectedly permits a direct prod write. No privileged production binding deletion is used.
ChangeRequests are audit records and are **never deleted**. Each justification
and note contains the run id printed at test start. An interrupted process may
leave run-owned objects: locate that run's records, complete approved production
decommission first, then remove only its exact nonprod names with the cleanup
identity. Do not delete audit records during recovery.

## Optional network and kubectl checks

Supply both organizations' nonprod **user** tier kubeconfigs, writable probe
namespaces, a probe image with `sh`, `sleep`, `nc`, literal B Postgres/API
`IP:port` addresses and a reachable A positive-control address. The suite starts
run-labeled pods in A and B. A must reach its control but cannot reach B's two
addresses; B must reach both targets, ruling out dead services and DNS failures.
Pods are removed by their tier identity. An image/pod/command failure fails the
test rather than counting as network isolation.

With `managementKubeconfig`, only its server address and CA are used. A's tier
credentials then attempt to list B's tenant namespace in management. Only an
explicit Forbidden/Unauthorized is accepted; TLS/network failures are failures.
Missing tier kubeconfigs skip this optional subtest; missing management config
skips only the kubectl portion. A full scenario 10 run requires all optional
inputs and MCP plus browser execution; skipped checks are not passes.

## Manual CI

The workflow is `workflow_dispatch` only. Configure the protected `acceptance`
environment and a runner with stand reachability. Secrets:

- `AEP_E2E_CONFIG`: private YAML; `${E2E_DIR}` expands to its temporary directory.
- `AEP_E2E_FILES`: JSON map of simple filenames to base64-encoded helpers and
  kubeconfigs. Helpers must run on the Linux runner. All files are private and
  removed in an `always()` step.
- `AEP_UI_ENV`: JSON string map of browser environment variables (see UI README).

The workflow serializes runs, typechecks the browser package, installs Chromium
and runs both suites. Disable the UI input only for a deliberate API-only run.
No secret files, traces or response bodies are uploaded as artifacts. Unit
checks in development use loopback fakes; no stand was contacted to author this
suite.
