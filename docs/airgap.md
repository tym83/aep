# Installation and upgrades without Internet access

AEP mirrors OCI charts and container images using one registry prefix. For example,
`ghcr.io/tym83/aep-bridge` becomes
`registry.example.com/aep/ghcr.io/tym83/aep-bridge`. Upstream hostnames remain path
components to avoid collisions. A source port is encoded as `_`, and Docker Hub
short names are expanded to `docker.io/library/...` or `docker.io/...`.

## Scope and prerequisites

The inventory covers the charts in `charts/` and `policies/`, their rendered
containers (including hooks and init containers), the operator's pinned Kyverno,
kgateway and OpenChoreo data-plane charts, and standalone/controller-created
images in `release/extra-images.yaml`. Both production and nonproduction policy
bundles are included. ServiceClaim CRDs and bootstrap RBAC are embedded in the
operator; they do not require a GitHub download. The optional LLM and SRE charts
are included, but model weights must be staged separately on their PVC.

This repository currently expects an existing OpenChoreo control plane. It does
not install Argo CD, ESO, Argo Workflows or the OpenChoreo observability/control
planes. Inventory and mirror those separately if your installation uses them.
Cozystack, Kubernetes, cert-manager, CNI, storage and Flux are prerequisites, not
AEP artifacts: follow [Cozystack's air-gap guide](https://cozystack.io/docs/install/kubernetes/air-gapped/) for
its air-gap installation and registry configuration for your pinned version.
Build-time dependencies in Dockerfiles (Node, apt, Yarn, Go modules and base
images) are not runtime artifacts. Build and publish AEP images on the connected
build network; disconnected rebuilding is a separate workflow.

Use a registry supporting OCI manifests, Helm media types and multi-platform
indexes. Configure nodes/container runtimes to trust its CA and authenticate
pulls in **every** management, control-plane and tier cluster. Flux credentials
alone do not authenticate kubelet image pulls. Prepare the chart source Secrets
in aep-system; the operator copies the configured Secrets into each managed
tenant namespace before creating OCIRepository objects. `secretRef` is a Docker registry credentials Secret; `certSecretRef`
contains the CA (and client certificate/key if mTLS is required) in the format
supported by your installed Flux source-controller.

## Prepare an immutable release

Run from the repository root on a connected machine with Go and Helm 3 installed:

```sh
go build -o bin/aep-release ./cmd/aep-release
bin/aep-release manifest
bin/aep-release manifest --check
```

Generation resolves real digests; any missing image/chart or signature discovery
error fails generation. It renders upstream charts by digest using the operator's
values, and local charts with representative required identity values. It packages
local charts deterministically into `release/artifacts/` (an OCI layout), so new
chart changes never reuse a previously published chart's digest. Commit this
layout alongside `release/manifest.yaml`; keep it with the CLI when mirroring.
Run commands from the repository root. `--root` changes inventory input paths;
manifest and local layout paths resolve relative to --root. Bundle and extraction
paths are explicit CLI paths relative to the working directory.

`manifest --check` is an offline freshness check over delivery code, charts,
policies, tooling, the standalone image list and build inventory. It does not
refresh mutable upstream tags. Run generation for each release after publishing
its images, then review the manifest diff and promote the exact digests. The
current chart image defaults use published development tags where historical
`0.1.0` image tags did not exist. A production release must publish images built
from its source revision and select those references before generation; this
branch does not publish images. Mirror repositories must enforce immutable tags,
since Helm values and Flux sources still select version tags.

Credentials use the standard Docker credential keychain (`docker login` and
credential helpers). Use `--ca /path/to/registry-ca.pem` for a private registry CA.
`--insecure-registry=registry.example.com:5000` enables HTTP for that host only; it does not disable HTTPS certificate verification.

```sh
bin/aep-release mirror --to registry.example.com/aep --ca registry-ca.pem --key vendor.pub
```

Copying retains all index children, original manifest bytes, digests and version
tags. Repeating a copy skips matching destination manifests and reuses blobs;
an existing tag with different content fails unless --allow-retag is explicitly set.

## Fully disconnected transfer

On the connected machine:

```sh
bin/aep-release bundle --out release/transfer --key vendor.pub
```

Transfer the complete directory and the binary through your approved process.
On the isolated side:

```sh
./aep-release load --from transfer --to registry.example.com/aep \
  --ca registry-ca.pem --key vendor.pub
```

The directory contains `layout.tar`, `manifest.yaml` and `checksums.json`. Loading
verifies both file checksums before extraction or registry writes, rejects unsafe
archive paths and links, and verifies recorded signatures before copying. The
loader needs only the directory and destination registry; it never contacts an
upstream registry. Preserve the manifest and checksums through a trusted channel:
checksums detect corruption, but do not authenticate a release inventory.

Cosign simple-signing artifacts at the conventional `sha256-<digest>.sig` tag are
discovered and pinned during generation, copied with their subjects, and verified
against the independently supplied PEM public key (ECDSA, RSA or Ed25519).
With --key, every artifact must have a valid signature, including artifacts whose
signature field is missing. Checksums supplied inside the bundle never establish
trust. Use repeated --trust-key=registry/prefix=vendor.pub for per-prefix trust
and --verify=registry/prefix,other/prefix to require verification explicitly.
Prefix boundaries are respected; the most specific trusted key wins. Unknown
or keyless signatures are copied without verification when their artifact is
not required by policy; required artifacts fail closed without a trusted key.
Keyless verification and OCI-referrer-only discovery are unavailable offline.

Load extracts into the system temporary directory by default. --work-dir selects
a writable parent (the bundle can be read-only); --max-size caps total extracted
bytes, defaulting to 100 GiB. Archive links and path traversal are rejected.

## Install

Follow [install.md](install.md), using your release's chart versions and images.
Use the same registry prefix for every AEP chart:

```yaml
global:
  imageRegistry: registry.example.com/aep
registrySecret: mirror-auth
registryCASecret: mirror-ca
```

The last two values belong to the operator chart. They configure Flux
`OCIRepository.spec.secretRef` and `.certSecretRef`. Create the referenced Secrets
in aep-system before onboarding an organization. They are copied with organization ownership checks. The operator flags
are `--image-registry-mirror`, `--chart-registry-mirror`, `--registry-secret` and
`--registry-ca-secret`; both mirror flags normally use the same prefix. For an HTTP chart mirror, set
registryInsecure: true (operator --registry-insecure).

```sh
helm upgrade --install aep-operator \
  oci://registry.example.com/aep/ghcr.io/tym83/aep/charts/aep-operator \
  --version 0.1.1 --namespace aep-system --create-namespace \
  -f operator-values.yaml -f mirror-values.yaml
```

Helm itself needs registry login and CA trust on the administration machine.
Apply `global.imageRegistry` to the change, read, bridge, ledger, SRE and LLM
charts too. Overrides must use the original source references; the helper
preserves their tag or digest and handles already mirrored references idempotently.
Operator delivery configures each upstream chart's actual registry fields,
including Kyverno hooks and kgateway's dynamically created proxy images.

Configure organization policy registry allowlists and production platform image
trust for the **mirrored** names. Copying signatures preserves their signed digest;
it does not make a private mirror a trusted signing authority. Supply your vendor
public keys to production policy values. Private image verification also needs
Kyverno registry credentials and CA trust configured in the target cluster.

## Upgrade and verify

1. Generate the new release manifest on the connected build network after its
   images are published. Compare `git diff OLD..NEW -- release/manifest.yaml`.
   Review added/removed artifacts, chart versions, image digests and signatures.
2. Mirror or bundle/load the new manifest before changing running releases.
   Keep old digests and charts for rollback; the CLI never deletes old artifacts.
3. Upgrade management releases, then verify a canary organization before upgrading
   other organizations. Review CRD changes explicitly as described in install.md.
4. Check Flux source and HelmRelease readiness, image pull events, bridge readiness,
   data-plane agent connectivity and Kyverno policy results. Test both an install
   and an upgrade with public egress denied. These are live acceptance checks;
   the local test suite does not claim a cluster acceptance run.

For automated local verification:

```sh
go build ./... && go vet ./... && go test ./...
bin/aep-release -h
bin/aep-release manifest --check
```

Transfer tests use an in-memory registry, copy multi-platform indexes and Helm
artifacts, repeat operations, load with the original registry stopped, and reject
checksum and signature failures. CI also fetches the pinned upstream charts and
renders their mirrored values, scanning workloads and hooks for public references.

## Troubleshooting

- `MANIFEST_UNKNOWN`: publish the selected image/chart version first. Never insert
  a fabricated digest or substitute a different release silently.
- `manifest is stale`: regenerate after delivery/chart changes and review the diff.
- `x509`: configure trust independently for the CLI, Helm, Flux and node runtimes.
- `401/403`: check CLI keychain credentials, Flux source Secrets, and node pull
  credentials separately. Authorization failures are never treated as absent data.
- `ImagePullBackOff`: compare the full host-preserving repository path and the
  requested tag/digest against the manifest, including kgateway proxy images.
- Signature failure: check the independently distributed vendor key, subject digest
  and copied signature artifact; do not disable production signature policy.
- A controller still contacts an external service: registry mirroring handles
  artifacts, not OIDC, OpenChoreo API endpoints, DNS, model downloads or application
  traffic. Configure those services for the isolated network as well.


## Release signing in CI

Set repository secrets COSIGN_PRIVATE_KEY (cosign private key PEM) and
COSIGN_PASSWORD. Images and exact deterministic chart OCI layouts are signed by
digest with conventional cosign signature tags. Signing steps check that the key
is present, so forks without it can build without signing. Distribute the public
key independently. Do not put private keys into bundles or chart values.
The charts workflow runs aep-release publish-charts; republishing a version with
different content fails and requires a version bump. It never silently skips a
conflicting version or repackages the chart through helm package.
