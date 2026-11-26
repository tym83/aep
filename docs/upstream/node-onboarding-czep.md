# CzEP draft: trusted machine onboarding and safe retirement

Status: proposal for Cozystack upstream; no AEP node controller is implemented.
Owner decision: machine lifecycle belongs to Cozystack. AEP supplies optional
approval workflow and CMDB integration, not Talos configuration or BMC control.

## Motivation

The existing bootbox stack combines Smee reservation mode and matchbox. Machines
must be listed in values, Talos configuration is applied using talm, storage pools
are prepared separately, and retirement is a manual sequence. Cloud tenant
clusters already use Cluster API/autoscaling. The missing capability is safe,
repeatable onboarding of physical management-cluster workers and storage nodes.
This proposal follows the internal `research_notes/node-onboarding.md` and DESIGN
5.15; discovery behavior and Talos integration need upstream spikes.

## Goals and non-goals

Discover machines without editing per-machine Helm values; attest identity before
releasing cluster secrets; select approved hardware into a pool; preserve storage
replication and etcd quorum during retirement; make every transition observable
and auditable. Support a local machine inventory and export references to CMDB.

Do not replace initial Cozystack installation, create a global PXE network, make
Omni mandatory, add a second Cluster API lifecycle for tenant clusters, or put
Talos/BMC credentials in AEP. Fleet API federation is distinct from hardware
inventory. Until upstream accepts a global catalog, site GitOps repositories own
pool definitions and hardware allow-lists.

## API sketch

Cluster-scoped `cozystack.io/v1alpha1` resources:

```yaml
kind: MachinePool
spec:
  role: worker
  selector:
    serialAllowList: [purchase-order-verified-serial]
  talos:
    image: factory.example/talos/approved-schematic:v1.13.0
    presetRef: cozystack-release
    patches: []
  storage:
    devices: [/dev/disk/by-id/approved-empty-device]
    poolType: lvmThin
    poolName: data
  approvalPolicy: manual
  bmcCredentialsRef: rack-3-redfish
---
kind: Machine
spec:
  poolRef: workers
  identityRef: supplier-attestation-record
  approvalRef: signed-decision-uid
  desiredState: Ready
status:
  phase: Quarantined
  conditions: []
```

Use `MachinePool`, not `NodePool`, to avoid confusion with tenant KubernetesNodes.
Schema details, image signing and discovery fields are upstream API review topics.
Inventory records serial, UUID, CPU/GPU, disks, NICs, BMC reference and site, with
source and observation timestamp. Status must not contain passwords or Talos
configuration. CRDs use map lists keyed by condition type and stable hardware IDs.

## Discovery and trust

States: Discovered → Identified → Approved → Configuring → Joined → StorageReady
→ Ready; unknown/mismatched identities enter Quarantined. A known, independently
verified allow-listed worker may follow an explicitly approved automatic policy;
unknown machines receive no cluster configuration. The stage-5 sentence that
says an unknown machine joins while a known machine is quarantined appears
reversed relative to the trust model and should be corrected upstream.

Run bootbox and the node controller locally per site. Spike Smee auto-proxy,
Tinkerbell auto-enrollment and permitted-subnet discovery; current reservation
mode must not be assumed to enroll unknown MAC addresses. A nonsecret discovery
image may boot unknown hardware into maintenance/quarantine without editing values.
Use a separate boot VLAN, DHCP snooping and, where supported, 802.1X.

A self-reported MAC/serial is evidence, not authentication. Read serial/UUID
independently over Redfish from the management network and compare with the
procurement allow-list. Use revocable discovery-image tokens. Regulated pools
require TPM endorsement/attestation against supplier keys, measured boot and
Secure Boot. Replayed evidence, changed disks/identity or failed attestation
return the machine to quarantine and require a new decision.

Only after identity verification and approval does the local controller render
Talos configuration with a reusable talm library and the Cozystack release preset.
Deliver through the Talos API over the protected bootstrap network; never publish
cluster CA material at a MAC-indexed HTTP URL. Maintenance API's unauthenticated
window remains a risk; constrain its reachability, bind the decision to verified
hardware and prefer attested/encrypted bootstrap channels as available. Secrets
stay local and are fetched for the operation from a secret manager.

## Approval integration

Without AEP, tenant-root super-admin approves through Cozystack. With AEP enabled,
admission permits Machine approval/decommission transitions only to the dedicated
change executor identity. `machine-approve` references Machine UID, identity hash,
pool revision, expiry, initiator and independent approver; retries are idempotent
and stale identity/pool revisions require new approval. The interface returns an
audit correlation ID. AEP must not write `status` or treat discovery as approval.
Control-plane additions/replacements always require manual approval; maintain
three or five healthy members and never automate a quorum-breaking transition.

## Storage and retirement

Choose disks by stable by-id references, capacity and identity, not `/dev/sdX`.
Never wipe an unknown, nonempty or previously joined disk automatically. Require
explicit destructive approval for reuse. Provision pools declaratively through
Piraeus `LinstorSatelliteConfiguration.spec.storagePools` selected by pool labels.
Wait for satellite, pool and replication health before StorageReady.

Retirement states: DrainRequested → Evacuating → Draining → Decommissioned.
First fence new placement and check capacity/failure domains for **every** affected
volume. Start LINSTOR evacuation; wait for replacement replicas to become UpToDate
and the configured replica count and placement constraints to be satisfied.
A diskful replica count alone is insufficient: verify synchronization and absence
of degraded/offline resources. Block on insufficient capacity, disconnected
satellites, snapshot dependencies or loss of quorum; timeouts raise an alert and
retain the machine. Respect PDBs while draining workloads. Recheck storage health
after rescheduling and before removing any replica, resetting Talos or powering off.
For control-plane machines, verify surviving etcd quorum before member removal.
Then reset, revoke credentials, remove Kubernetes Node membership and optionally
power off through Rufio/Redfish. Persist checkpoints for restart-safe retries.
An emergency override is a separately audited data-loss decision, not an automatic
fallback on timeout. Test power failure and controller restart at every boundary.

## Alternatives

Omni provides a coherent machine workflow but introduces production licensing and
Talos ownership changes; support existing installations as an optional backend.
Metal3/Ironic has strong hardware management but adds operational weight and needs
a validated Talos integration. Full Tinkerbell workflows duplicate Talos's installer;
reuse Smee/Hardware and optionally Rufio instead. Matchbox HTTP configuration is
simple but an unsafe distribution channel for cluster secrets. Self-hosting the
management cluster under CAPI is a broader installation redesign.

## Risks and acceptance experiments

Prototype unknown-machine discovery, independent identity verification, TPM
attestation and a talm rendering library. Test spoofed serials, replayed approval,
compromised boot VLAN, duplicate identities and secret revocation. Prove a valid
machine can join without values edits, unknown hardware remains quarantined,
and storage retirement cannot proceed while any required replica is unsynchronized.
Verify failure recovery with controller crashes, unreachable BMC, full storage and
an unhealthy etcd member. Upstream must decide catalog ownership, API naming,
supported Talos/Piraeus releases and attestation evidence formats before coding.

References: [Cozystack scaling](https://cozystack.io/docs/operations/cluster/scaling/),
[talm](https://github.com/cozystack/talm),
[Piraeus API](https://piraeus.io/docs/stable/reference/linstorsatelliteconfiguration/),
[Tinkerbell](https://github.com/tinkerbell/tinkerbell).
