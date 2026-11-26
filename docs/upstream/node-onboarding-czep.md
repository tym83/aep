# CzEP draft: trusted machine onboarding and safe retirement

Status: proposal for Cozystack upstream; the node controller is not implemented.
Machine lifecycle, Talos configuration and BMC control belong to Cozystack.

## Motivation

The existing bootbox stack combines Smee reservation mode and matchbox. Machines
must be listed in values, Talos configuration is applied using talm, storage pools
are prepared separately, and retirement is a manual sequence. Cloud tenant
clusters already use Cluster API/autoscaling. The missing capability is safe,
repeatable onboarding of physical management-cluster workers and storage nodes.
Discovery behavior and Talos integration require the acceptance experiments below.

## Goals and non-goals

Discover machines without editing per-machine Helm values; attest identity before
releasing cluster secrets; select approved hardware into a pool; preserve storage
replication and etcd quorum during retirement; make every transition observable
and auditable. Support a local machine inventory and export references to CMDB.

Do not replace initial Cozystack installation, create a global PXE network, make
Omni mandatory, add a second Cluster API lifecycle for tenant clusters, or put
Talos/BMC credentials in an external approval system. Fleet API federation is distinct from hardware
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
unknown machines receive no cluster configuration. All unknown or mismatched machines remain quarantined.

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

Use the **Keylime agent in the Linux discovery image**, with a site-local registrar
and verifier. Pin the discovery image, Secure Boot keys and measured-boot policy;
do not use an accept-all policy. Validate TPM EK provenance, a fresh nonce-bound
quote and measured-boot log against approved firmware/image measurements, binding
the TPM identity to the independently collected hardware record.

The proposed controller consumes verifier results over authenticated TLS, never a
node-written label. It records a short-lived authorization containing Machine UID,
TPM identity hash, boot nonce, policy digest, pool revision and expiry. A matching
human approval plus a current passing result is the gate to release a one-use,
TPM-bound install payload. Failure, timeout, replay or identity drift denies the
payload and returns Quarantined. Keylime supplies attestation and secure payload
primitives; this authorization adapter and its Talos installer integration remain
prototype work, not an existing Cozystack feature.

Only that authorized discovery session may download the signed Talos installer
and encrypted machine configuration rendered by a talm library. Verify the image
and configuration digest before installation; discard the token after use. Never
publish a reusable Talos configuration at a MAC-indexed URL. A reboot into Talos
ends the discovery attestation session: separately verify the resulting node
identity and configured boot chain before declaring Joined. Do not claim that a
discovery quote proves the later Talos runtime. A failed install requires a fresh
attestation and authorization; secrets remain in the local secret manager.

## Approval integration

By default, tenant-root super-admin approves through Cozystack. When integrated
with an external approval system,
admission permits Machine approval/decommission transitions only to the dedicated
change executor identity. `machine-approve` references Machine UID, identity hash,
pool revision, expiry, initiator and independent approver; retries are idempotent
and stale identity/pool revisions require new approval. The interface returns an
audit correlation ID. The external approval system must not write `status` or treat discovery as approval.
Control-plane additions/replacements always require manual approval; maintain
three or five healthy members and never automate a quorum-breaking transition.

## Storage and retirement

Choose disks by stable by-id references, capacity and identity, not `/dev/sdX`.
Never wipe an unknown, nonempty or previously joined disk automatically. Require
explicit destructive approval for reuse. Provision pools declaratively through
Piraeus `LinstorSatelliteConfiguration.spec.storagePools` selected by pool labels.
Wait for satellite, pool and replication health before StorageReady.

Retirement states: DrainRequested → Evacuating → Draining → Decommissioned.
Fence new placement and check capacity/failure domains for every affected volume.
Use the Piraeus **v2** lifecycle: explicitly configure
`LinstorSatelliteConfiguration.spec.deletionPolicy: Evacuate` before requesting
`LinstorSatellite` deletion; recent v2 releases default to Retain. Prevent automatic
recreation by excluding the retiring node from satellite selection. The machine
controller must cooperate with Piraeus finalization, not run a second competing
LINSTOR evacuation or remove finalizers. Coordinate PDB-aware drain and PV
reattachment with the Piraeus pre-drain/pre-terminate hooks when CAPI is present.
Without CAPI, implement equivalent checkpoints and wait for successful evacuation
before reset/power-off; a timeout is not completion.

Check **diskful, diskless and TieBreaker** resources, connections and snapshots.
For each 2-replica DRBD volume, record the configured quorum policy and verify the
surviving voting topology. A diskless TieBreaker on a third failure domain may be
needed to retain majority after one diskful replica fails; diskless does not mean
irrelevant to quorum. Verify active connections, UpToDate data replicas, replacement
placement and the intended `on-no-quorum` behavior. Two healthy diskful copies alone
do not prove that the next removal preserves write availability. Do not disable
quorum or silently switch to `Delete` to make retirement succeed. Block on degraded
replicas, unsafe quorum, insufficient space, offline satellites or pending snapshots.
Recheck after reattachment and before deleting any remaining storage identity.
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

Attestation reference: [Keylime agent](https://keylime.readthedocs.io/en/latest/man/keylime_agent.8.html).
Lifecycle reference: [Piraeus satellite deletion policies](https://piraeus.io/docs/stable/explanation/deletion-policies/).
