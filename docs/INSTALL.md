# Install and operate SpawnPod v0.1.0-rc.6

For the complete first-run sequence, resource preflight, private Runner Secret
creation, image pull troubleshooting and a real workflow, use the
[Forgejo + K3s quickstart](QUICKSTART-K3S.md).

Supported platform: Linux/amd64, one active controller. Start with a test
cluster. Review the [security model](SECURITY.md) and
[validation results](VALIDATION.md).

## Prerequisites

- Kubernetes with stable ValidatingAdmissionPolicy, Restricted Pod Security,
  an enforcing NetworkPolicy implementation, and sufficient ephemeral storage.
  The tested baseline is K3s v1.34.12+k3s1.
- An existing Forgejo instance and a registered official Runner v13.1.0.
  See [official Runner installation](https://forgejo.org/docs/latest/admin/actions/installation/docker/)
  and [registration](https://forgejo.org/docs/v15.0/admin/actions/registration/).
- Administrator access for the one-time namespace/RBAC/admission installation.
  Runner never receives this credential.
- Approved job images containing the workflow's tools, runnable with a numeric
  non-root UID and a read-only root filesystem. Node actions need Node in the
  job image. The release's scratch image is the controller/agent runtime, not
  a general job image.
- A separately operated source proxy if using `network: source`. Its fixed
  endpoint must be reachable from Pods and its approved destination routes
  must be maintained. The isolated default profile needs no source proxy.

## Verify the downloaded package

Download `SHA256SUMS` and every file it lists from the same release. Choose
the archive for your host architecture. Run these commands from the download
directory (amd64 shown).

```sh
sha256sum -c SHA256SUMS
tar -xzf spawnpod_v0.1.0-rc.6_linux_amd64.tar.gz
cd spawnpod_v0.1.0-rc.6_linux_amd64
sha256sum -c BINARY_SHA256SUMS
./spawnpod version
./spawnpod-agent version
./spawnpod-source-proxy version
```

All three commands should print `v0.1.0-rc.6`. Checksums detect corruption.
Archives created by the tag workflow have GitHub build and SBOM attestations;
verify one with `gh attestation verify ARCHIVE -R dmedovich/SpawnPod`.
`release.json` identifies the source revision and whether it was dirty. Published packages must have
`dirty: false`.

## Prepare runtime and Runner images

Releases built by the current tag workflow publish a multi-platform runtime
image at `ghcr.io/dmedovich/spawnpod:VERSION`. `GHCR_DIGEST` records its
immutable reference, which is signed using GitHub OIDC and carries SBOM and
provenance attestations. See [release verification](RELEASE.md#publish).
Alternatively build from this extracted package and push to your own registry:

```sh
podman build --build-arg VERSION=v0.1.0-rc.6 \
  -f deploy/release/runtime.Dockerfile -t registry.example.com/spawnpod:v0.1.0-rc.6 .
podman push registry.example.com/spawnpod:v0.1.0-rc.6
```

Record the registry manifest digest and use the immutable
`registry.example.com/spawnpod@sha256:...` reference. The same runtime image
provides `/spawnpod`, `/spawnpod-agent` and `/spawnpod-source-proxy`.
Do not copy a local OCI index digest into a registry reference without
checking that the registry preserves that digest.

For an offline single-node K3s installation, import the downloaded OCI
archive, inspect its manifest digest, and tag the exact named digest:

```sh
sudo k3s ctr -n k8s.io images import --digests ../spawnpod_v0.1.0-rc.6_linux_amd64.oci.tar
sudo k3s ctr -n k8s.io images list
# Replace DIGEST below with the manifest digest recorded in the release.
sudo k3s ctr -n k8s.io images tag localhost/spawnpod:v0.1.0-rc.6 \
  localhost/spawnpod@sha256:DIGEST
```

Import/tag on every eligible node, or use a reachable registry. Obtain the
[official Runner image](https://forgejo.org/docs/latest/admin/actions/installation/docker/)
for version 13.1.0, verify `/bin/forgejo-runner --version`, record its digest
and make it available to the cluster. SpawnPod does not redistribute the
Runner binary in its package.

## Configure and render offline

Copy `deploy/config.example.yaml` to an operator-owned file. Replace both
sample execution image digests, select explicit resource budgets and keep
services disabled until tested. `agentImage` points at the released runtime
image. Define `sourceProxy` plus a source profile only when its proxy and
network policy can be tested. The example budgets are placeholders, not a
capacity recommendation for your hardware.

```sh
./spawnpod validate --config operator.yaml
./spawnpod render-admission --config operator.yaml > admission.yaml
./spawnpod render-deployment --config operator.yaml \
  --controller-image registry.example.com/spawnpod@sha256:YOUR_RUNTIME_DIGEST \
  --runner-image data.forgejo.org/forgejo/runner@sha256:YOUR_RUNNER_DIGEST \
  > installation.yaml
```

Use full 64-character lowercase hexadecimal digests. The install renderer is
for the `infra`/`ci` reference layout; it rejects other namespace/Lease names.
It sets the in-cluster token configuration, private socket path and `:9090`
monitor address. It deliberately omits any host kubeconfig path.

Copy `deploy/release/runner.example.yaml` to private `runner.yaml`, mode 0600.
Fill the registered UUID/shared secret and the reachable Forgejo HTTPS URL.
Its label image must match the selected SpawnPod profile exactly. Use trusted
CA certificates in the Runner image for private PKI; never disable TLS
verification. Job image CA trust is a separate requirement.

## Install

Review all rendered manifests before applying them. With administrator access:

```sh
kubectl apply -f deploy/security.yaml
kubectl apply -f deploy/network-default-deny.yaml
kubectl apply -f admission.yaml
kubectl -n infra create secret generic spawnpod-runner --from-file=runner.yaml
kubectl apply -f installation.yaml
kubectl -n infra rollout status deployment/spawnpod --timeout=180s
```

The Secret command is for a first install and refuses an existing Secret;
update it explicitly during credential rotation. The controller receives a
projected rotating ServiceAccount token. Runner has no token mount, receives
only its registration Secret, and waits for the controller's socket before
starting. Both containers run with UID/GID 10001 and a read-only rootfs.

The external source proxy has its own lifecycle and credentials policy. The
release includes a route config example; a source profile fails preflight
unless the exact corresponding egress policy is installed. This candidate
does not deploy the proxy automatically or grant arbitrary DNS/internet access.

## Verify the installation

```sh
kubectl -n infra logs deployment/spawnpod -c controller
kubectl -n infra logs deployment/spawnpod -c runner
kubectl -n infra exec deployment/spawnpod -c controller -- \
  /spawnpod doctor --config /etc/spawnpod/config.yaml
kubectl -n infra exec deployment/spawnpod -c controller -- \
  /spawnpod smoke --config /etc/spawnpod/config.yaml
kubectl -n infra port-forward service/spawnpod-monitor 9090:9090
# In another terminal:
curl --fail http://127.0.0.1:9090/healthz
curl --fail http://127.0.0.1:9090/readyz
curl --fail http://127.0.0.1:9090/metrics
```

Then dispatch a real Forgejo workflow with sequential steps, file transfer
and cancellation. If enabled, test service isolation and source checkout.
The source repository includes
`tests/integration/check-cluster-policy.py` and an opt-in K3s acceptance suite.
See its [test instructions](https://github.com/dmedovich/SpawnPod/tree/main/tests/integration). `doctor`
and a healthy Pod alone do not establish isolation.

Do not expose monitoring to the public internet. Restrict management access
using your cluster networking controls. Scrape RPC latency sum/count and
reconciliation failures; investigate increasing failures or stale successful
reconciliation timestamps. Production capacity and SLOs must be measured on
the target node before raising job concurrency.

## Upgrade, rollback and credentials

1. Quiesce Runner scheduling and wait for active workflows/environments to
   finish and confirm all job/service resources have been removed. RPC drain
   does not finish an entire workflow across multiple steps.
2. Save current operator config, manifests, image digests and non-secret
   environment records. Keep registration credentials in your secret store.
3. Render and review the new admission policy and install manifest. Apply the
   policy before starting a controller that requires its new image allowlist.
4. Apply the install manifest. Recreate and Lease election ensure one active
   controller; config checksum changes trigger a new Pod. Repeat doctor,
   smoke and a real workflow before re-enabling scheduling.
5. For rollback, restore the matching old config, admission, NetworkPolicy and image digests
   together, then rerun checks. The local rc.1/rc.2 controller compatibility
   check uses drained records and the current agent image; it does not migrate
   running jobs or make the old vulnerable agent safe. Record schemas 1 and 2
   are readable; unknown
   future schemas fail closed. Do not downgrade across an untested schema
   change or delete records to force startup.
   Controllers through rc.4 require the original default-deny selector
   `spawnpod.dev/managed: "true"`; restore that baseline while drained before
   rolling back from a namespace-wide policy. Omit `tombstoneRetention` from
   configs used by older controllers, which reject unknown fields.
6. Rotate Runner credentials by replacing the private Secret and restarting
   the Deployment while quiesced. Kubernetes projected tokens rotate
   automatically. Review source-route pins and CA expiry independently.

For diagnosis use `environments list`, `environments inspect ID` and
`environments cleanup --dry-run` with the configured identity. The last
command is a preview; this candidate has no destructive manual cleanup CLI.
The controller retries owned cleanup. Restore API access or restart the
controller to reconcile interrupted environments; never remove unverified
foreign resources. Uninstall only after jobs are drained and resources
cleaned, and preserve state until recovery is no longer needed.

Cleaned ConfigMaps are durable retry tombstones. They retain only management
metadata and expire after `controller.tombstoneRetention` (default `24h`,
configurable from `1m` to `24h`) on a successful reconcile sweep. For small
ConfigMap quotas choose a shorter retention or a budget sized for the number
of jobs completed during that period. `inspect` reports `retained_until`, Pod
presence and the record-level cleanup decision. `doctor --strict` fails on
quota headroom or local-image warnings; `doctor --installation` additionally
checks the Runner Secret key, mounted config and infra quota with operator
credentials. See the quickstart for identity and quota limitations.

## Upcoming declarative upgrade tooling

The rc.6 upgrade workflow generates and checks a coordinated installation
bundle. See [UPGRADES.md](UPGRADES.md) for phase gates, explicit GitOps application,
image verification limits and recovery. These commands are not in rc.5 binaries.
