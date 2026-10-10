# Implementation step 21 — local first-run kit

**Status:** the local kit was exercised on K3s and Forgejo on 2026-10-09.
Results and remaining limits are recorded in [step 22](22-local-live-validation.md).

## What was prepared

[`deploy/smoke/prepare-local.sh`](../../../deploy/smoke/prepare-local.sh)
builds the controller and agent binaries and, when the supplied local Runner
source exists, the Runner binary. It builds an OCI agent image with Podman,
pulls an amd64 Alpine image for a small shell-only workflow, and writes the
following ignored files under `.local/`:

- `bin/spawnpod`, `bin/spawnpod-agent`, `bin/spawnpod-recovery-client`,
  `bin/forgejo-runner`
- `config.yaml` with image digests, a private socket path, a local kubeconfig
  path and a small namespace quota
- `runner.generated.yaml` with the same image in its `spawnpod` label and the
  local plugin socket; the first run copies it to `runner.yaml`, where
  `server.connections` must be filled after registration. Later generator runs
  preserve `runner.yaml` so the local Runner credentials are never overwritten.
- `spawnpod-agent.oci.tar` for import into K3s containerd

The local Runner archive has no release metadata and builds as `version dev`.
Its `plugin.proto` matches the copy used by SpawnPod byte for byte (SHA-256
`961fc5fc541c5c79f5502632d9f6dd3daa110ecfd55b9fbf697778b873367f79`).
This establishes a protocol schema match with that local source, not a
published Runner release compatibility claim.

## First-run sequence after tool preparation

1. Check free RAM/disk and Fedora networking; install single-node K3s with its
   built-in NetworkPolicy controller enabled. The reviewed
   [`install-host-k3s.sh`](../../../deploy/smoke/install-host-k3s.sh) pins
   [K3s v1.34.12+k3s1](https://github.com/k3s-io/k3s/releases/tag/v1.34.12%2Bk3s1)
   to match the namespace's Pod Security `v1.34` labels and the client library.
   It uses API port `744`, below the FedoraWorkstation zone's existing broad
   `1025-65535/tcp` allowance, and adds the default pod/service CIDRs to
   firewalld's trusted zone. Review these host-wide changes before running it
   as root. The downloaded installer is in ignored `.local/get-k3s.sh`; the
   root script checks its SHA-256 before execution. See
   [K3s requirements](https://docs.k3s.io/installation/requirements?os=rhel)
   and [network policy controller](https://docs.k3s.io/networking/networking-services).
2. Run `deploy/smoke/prepare-local.sh` again if any source or image changed.
   Compare `.local/runner.yaml` with `.local/runner.generated.yaml` and update
   its label if the image digest changed.
   Import `.local/spawnpod-agent.oci.tar` into containerd's `k8s.io`
   namespace, then tag the imported image with the exact named digest from
   `.local/config.yaml`. `--digests` alone did not create that named reference
   in this K3s/containerd combination. K3s documents
   [image importing](https://docs.k3s.io/add-ons/import-images). If the
   imported digest differs, stop and reconcile image references before Pods.
3. Copy K3s's admin kubeconfig into ignored `.local/kubeconfig`, mode `0600`,
   for the isolated smoke run. Keep this credential out of the repository and
   Runner config. Apply `deploy/security.yaml`,
   `deploy/network-default-deny.yaml`, and the output of
   `.local/bin/spawnpod render-admission --config .local/config.yaml`.
4. Run `.local/bin/spawnpod doctor --as-controller --config .local/config.yaml`.
   `--as-controller` uses Kubernetes impersonation of the
   `infra/spawnpod-controller` ServiceAccount so the checks exercise its RBAC.
   Run `serve` with the same flag from the same unprivileged host user; both
   commands require an admin kubeconfig authorized to impersonate that account.
   The Unix socket is mode `0600`, so Runner must use the same host UID.
5. Run `.local/bin/spawnpod smoke --config .local/config.yaml`. The client
   performs `Capabilities`, `Create`, `Start`, `CopyIn`, `Exec`, `CopyOut` and
   `Remove` over the actual Unix socket. It tries `Remove` with a fresh cleanup
   context even after a failed step. If Create succeeds but returns no ID,
   inspect records before retrying.
6. Start a local Forgejo container, complete onboarding, create a test
   repository and register a Runner. Populate the private `.local/runner.yaml`
   `server.connections` with Forgejo's URL, UUID and shared secret. The
   [Forgejo registration guide](https://forgejo.org/docs/v15.0/admin/actions/registration/)
   describes those values. Run the locally built Runner as the same host user
   and commit [`deploy/smoke/workflow.yml`](../../../deploy/smoke/workflow.yml)
   into the test repository's `.forgejo/workflows/` directory. Trigger it by
   `workflow_dispatch` and confirm both steps pass.
7. Inspect `.local/bin/spawnpod environments list --config .local/config.yaml`
   and `cleanup --dry-run`. Verify no job Pod remains after Remove and that
   tombstones expire after 24 hours. Next run negative admission, quota and
   network-isolation scenarios before enabling service containers.

From the repository root, the cluster application and first controller
commands are:

```sh
sudo deploy/smoke/install-host-k3s.sh
sudo k3s kubectl get nodes
sudo k3s ctr -n k8s.io images import --digests .local/spawnpod-agent.oci.tar
sudo k3s ctr -n k8s.io images tag localhost/spawnpod-agent:smoke localhost/spawnpod-agent@sha256:38783759e4fffff21ca43f80a8c0e299a0f24583312fc6ec0249baa5bfd0f09c
sudo k3s ctr -n k8s.io images list
umask 077
sudo cat /etc/rancher/k3s/k3s.yaml > .local/kubeconfig
sudo k3s kubectl apply -f deploy/security.yaml
sudo k3s kubectl apply -f deploy/network-default-deny.yaml
.local/bin/spawnpod render-admission --config .local/config.yaml > .local/admission.yaml
sudo k3s kubectl apply -f .local/admission.yaml
.local/bin/spawnpod doctor --as-controller --config .local/config.yaml
.local/bin/spawnpod serve --as-controller --config .local/config.yaml
```

Keep `serve` running in one terminal, then run the direct protocol scenario in
another:

```sh
.local/bin/spawnpod smoke --config .local/config.yaml
```

The Forgejo test container can then be started from the same host user:

```sh
podman volume create spawnpod-forgejo
podman run --detach --name spawnpod-forgejo \
  --publish 127.0.0.1:3000:3000 \
  --volume spawnpod-forgejo:/data \
  codeberg.org/forgejo/forgejo:15
```

After completing the web setup and registration at `http://127.0.0.1:3000`,
fill `.local/runner.yaml` and run
`.local/bin/forgejo-runner --config .local/runner.yaml daemon`. Keep its logs
in a separate terminal during the workflow run.

For the first Forgejo instance, a rootless Podman container bound only to
`127.0.0.1:3000` keeps this test local. Use an official Forgejo image and a
persistent named volume; see the [Forgejo container guide](https://forgejo.org/docs/v15.0/admin/installation/docker/).
The example Runner configuration disables cache and limits concurrency to one.
It deliberately uses a workflow without checkout or external actions, so the
first run exercises SpawnPod's core path before network access is introduced.

The smoke kubeconfig contains admin credentials on the host; the controller
impersonates its restricted ServiceAccount for API requests. This is a local
verification topology. A persistent deployment should use a projected
ServiceAccount token in a Pod and keep the runner without Kubernetes API
credentials.

Kubelet reads images from containerd's `k8s.io` namespace. The first import
used the default namespace, and later `--digests` still did not create the
named `localhost/spawnpod-agent@sha256:…` reference. Explicit `ctr images tag`
resolved `Init:ImagePullBackOff`; the direct RPC smoke then passed. Regenerate
the digest in the command above if the agent image is rebuilt.
