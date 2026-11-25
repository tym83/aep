# Change service

The change service governs every write into protected OpenChoreo
environments. In OpenChoreo authorization, only the per-organization client
`aep-change-<org>` may write release bindings in protected environments;
everyone else — portal users, CLI, agents, ITSM — requests a `ChangeRequest`,
a human (or a pre-approved standard template) approves it, and the change
controller executes it through the OpenChoreo REST API. Every change thereby
passes OpenChoreo's own authorization and audit, and AEP adds the approval
trail, separation of duties and serialization on top.

The service is a Tier-0 component: it holds credentials that write to every
organization's protected environments. Run it on dedicated nodes, give it no
inbound exposure beyond the API's audience, and audit every action.

## Components

| Component | Binary | Responsibility |
|---|---|---|
| Change API | `cmd/change-api` | OIDC authentication, request validation, idempotent creation, recording human decisions and notes. Never applies anything. |
| Change controller | `cmd/change-controller` | The state machine: validation, supersede, snapshots, approval evaluation, scheduling, lease-serialized idempotent applies with backoff. |

Both run on the management cluster. `ChangeRequest` objects live in the
organization's tenant namespace (`tenant-<org>` by default, configurable with
`--org-namespace-template`); the OpenChoreo namespace of an organization is
the organization name.

## ChangeRequest

`aep.aenix.io/v1alpha1`, namespaced, immutable spec. See
`config/crd/aep.aenix.io_changerequests.yaml`.

- `operation` — one of `promote`, `rollback`, `resource-update`,
  `resource-restore`, `credential-rotate`, `policy-exception`, `breakglass`,
  `organization-change`, `cluster-attach`, `machine-approve`,
  `machine-decommission`, `decommission`.
- `model` — `standard` (pre-approved template, runs immediately with a
  record), `normal` (eligible human approver), `emergency` (second person
  from security on-call).
- `target` — typed reference: `kind` (`Component`, `Resource`, `Project`,
  `Organization`, `Cluster`, `Machine`), `name`, `project`, `environment`.
  For binding operations the OpenChoreo binding kind is derived:
  Component → `ReleaseBinding`, Resource → `ResourceReleaseBinding`,
  Project → `ProjectReleaseBinding`; the binding name is the deterministic
  `<name>-<environment>`.
- `desired` — operation payload, e.g. `{"releaseName": "api-1.4.0"}` for
  promote/rollback.
- `requester`, `requesterGroups` — filled by the API from the caller's
  token, never from the request body.
- `window` — optional planned start/end. An approval is valid until the
  window end; without a window, 72 hours after creation (`--approval-ttl`).
- `externalRef` — ITSM identifier.

Status carries `phase`, `approvals` (user, groups, role, decision, time,
source `portal|itsm|system`, evidence), `notes`, the `before` binding
snapshots, a `rollbackPlan`, `attempts` and `conditions` (`Validated`,
`Approved`, `Applied`).

## State machine

```
Draft → Pending → Approved → Applying → Applied
           │         │  └────→ Scheduled ┘ (waits for the window)
           │         └──────→ Rejected
           ├───────────────→ Expired      (approval/pending past deadline)
           └───────────────→ Superseded   (a newer change for the same target)
                     Applying → Failed    (Stale, ApplyFailed)
```

Rules enforced by the controller:

- **Separation of duties.** The requester never approves their own change.
  Service accounts never decide. Application changes need a member of
  `aep-<org>-approver`; the platform team does not approve application
  changes. `organization-change` touching `access`, `quotas` or `profile`
  needs `aep-platform-team` or `aep-security`. `emergency` model and
  `breakglass` need a second person from `aep-security-oncall`; breakglass
  requesters must be members of `aep-<org>-breakglass`. Token group paths
  (`/teams/aep-acme-approver`) are normalized to the last segment.
- **Standard model.** Auto-approved and recorded with a `system` approval:
  rollback to a release previously applied to the same target,
  `credential-rotate`, and `resource-update` that only scales
  (`resourcesPreset`, `size`, `replicas`) within the policy ceiling
  (`--policy-config`, same format as the bridge policy; the ceiling is the
  most restrictive across configured kinds). Anything else under
  `model: standard` falls back to human approval with a
  `StandardNotEligible` condition.
- **One active change per target.** The deterministic target key (binding
  identity for binding operations) serializes changes; a newer request
  supersedes older pending/approved/scheduled ones. A change that is already
  applying finishes; the newer change's stale check then sees the result.
- **Stale detection.** While pending, the controller snapshots the target
  binding (`status.before`). Before every apply attempt the live binding is
  compared with the snapshot; any difference fails the change with reason
  `Stale` — the approval covered a state that no longer exists.
- **Lease serialization.** Applies hold a per-binding `Lease`
  (`aep-change-<key>`, 60 s) in the organization namespace.
- **Idempotent retries.** Apply converges and is safe to repeat; transient
  errors (network, 5xx, 429) retry with exponential backoff (5 s → 5 min)
  up to `--max-attempts` (default 8); permanent 4xx fails immediately.
- **Read-modify-write.** The OpenChoreo update replaces the whole binding
  spec without a precondition, so handlers patch the live spec and preserve
  fields they do not manage, and never touch bindings owned by someone else.

## Operations

| Operation | Status | Behavior |
|---|---|---|
| `promote` | implemented | Pins the release on the target binding; creates the binding when missing (prod `ResourceReleaseBinding` gets `retainPolicy: Retain`). |
| `rollback` | implemented | Same mechanics as promote; standard-model eligibility requires the release to have been applied to this target before. |
| `resource-update` | implemented | Replaces `resourceTypeEnvironmentConfigs` and/or advances `resourceRelease`/`retainPolicy` on an existing `ResourceReleaseBinding`. |
| `decommission` | implemented | Removes the protected binding (owner-checked) so the object itself can then be deleted; idempotent. |
| `breakglass` | records only | Validated (duration ≤ 4h, requester in the breakglass group), approved by security on-call, recorded with reason `Recorded`. The actual grant sits behind the `change.Granter` interface — a TODO: temporary group membership, expiring prod bindings, SIEM alert, post-window credential rotation. |
| the rest | stored, `NotImplemented` | Validated, approved and stored; the `Applied` condition reports `NotImplemented` until a handler lands. |

## HTTP API

Contract: `itsm/contract/openapi.yaml` (OpenAPI 3.1). Summary:

- `POST /v1/changes` — create; requires `Idempotency-Key`. Creator groups:
  `aep-<org>-developer|platform|admin`. Replays return the same change.
- `GET /v1/changes/{org}/{name}` — read; org members and platform roles.
- `POST /v1/changes/{org}/{name}/decision` — approve/reject; requires
  `If-Match: <resourceVersion>` (missing → 428, stale → 412), a human token,
  and an eligible group; one decision per user, only while `Pending`.
- `POST /v1/changes/{org}/{name}/notes` — comment; service accounts allowed
  (recorded as `source: itsm`).

```sh
curl -X POST https://change.aep.example/v1/changes \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"organization":"acme","operation":"promote","model":"normal",
       "target":{"kind":"Component","project":"shop","name":"api","environment":"prod"},
       "desired":{"releaseName":"api-1.4.0"},"justification":"JIRA-4811 fix"}'

curl -X POST https://change.aep.example/v1/changes/acme/chg-…/decision \
  -H "Authorization: Bearer $APPROVER_TOKEN" -H "If-Match: 4821193" \
  -d '{"decision":"approve","evidence":"CHG0001234"}'
```

## Configuration

`cmd/change-api`: `--issuer` (required), `--jwks-url` (defaults to the
Keycloak realm certs endpoint), `--audience`, `--username-claim`,
`--groups-claim`, `--listen`, `--tls-cert/--tls-key`,
`--org-namespace-template`, `--kubeconfig`.

`cmd/change-controller`: `--oc-api-url` (required), `--oc-token-url`,
`--oc-client-id-template` (default `aep-change-{org}`), `--oc-secret-name`
(default `aep-change-{org}`, a Secret in the organization namespace with
`client-secret` and optional `client-id`/`token-url` keys),
`--policy-config`, `--approval-ttl` (72h), `--max-attempts` (8),
`--leader-election-namespace`, `--org-namespace-template`,
`--instance-id`, `--kubeconfig`.

RBAC examples: `config/change/rbac.yaml`. The controller needs
changerequests (+status), leases and per-org Secrets; the API needs
changerequests create/read and status update. Neither needs anything else.

Tokens for the OpenChoreo API are client-credentials of `aep-change-<org>`,
cached per credential set; rotating the Secret takes effect on the next
reconcile without a restart.

## Failure modes

- **OpenChoreo unavailable** — applies retry with backoff; approved changes
  wait; nothing is lost. Expiry still applies.
- **Controller down** — requests accumulate in `Pending`; approvals are
  recorded regardless; execution resumes with the controller. Lease expiry
  (60 s) unblocks bindings after a crash mid-apply.
- **API down** — no new requests or decisions; the controller keeps
  executing what is approved. Direct kubectl access to `ChangeRequest` is
  governed by Kubernetes RBAC, not by this service.
- **Stale approval** — the change fails with reason `Stale` and must be
  re-requested; nothing is overwritten.

## Known limitations and open questions

- `resource-restore`, `credential-rotate`, `policy-exception`,
  `organization-change`, `cluster-attach`, `machine-approve`,
  `machine-decommission` return `NotImplemented`; `breakglass` records
  without granting (`change.Granter` is the extension point).
- Approval quorum is 1; per-organization quorum and ordered approvals
  (design 5.6) need the `Organization` CRD, which does not exist yet.
- Freeze windows are not enforced; they also need organization settings.
- The standard-model scale ceiling is the most restrictive across all
  configured kinds because a binding does not reveal its backend kind.
- Decisions via the API are always `source: portal`; `source: itsm`
  decisions require the signed-assertion flow (design 5.6), which is not
  implemented.
- `EndpointReleaseBinding` cleanup on decommission relies on OpenChoreo's
  own cascade; only the three main binding kinds are managed.
- There is no list endpoint; the portal lists ChangeRequests through the
  Kubernetes API.
