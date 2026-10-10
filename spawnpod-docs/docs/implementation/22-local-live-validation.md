# Implementation step 22 — first live K3s and Forgejo run

**Date:** 2026-10-09. **Host:** Fedora 43, single-node K3s v1.34.12+k3s1.
The node reported `Ready`. This is a local smoke result, not production
qualification.

## Topology and private files

- K3s runs on the host. `ci` has the restricted job ServiceAccount, default
  deny NetworkPolicy, ResourceQuota, LimitRange, and generated admission
  policy. `infra` holds the controller Lease and RBAC.
- The SpawnPod controller runs as the unprivileged host user with
  `--as-controller`. `.local/kubeconfig` contains a mode `0600` copy of the
  admin kubeconfig used only for this local impersonation test.
- Rootless Forgejo v15.0.9 runs in Podman with a persistent named volume and
  listens at `127.0.0.1:3000`. The same host user runs the locally built
  Forgejo Runner and connects to SpawnPod through the private Unix socket.
- `.local/forgejo-admin-password`, `.local/forgejo-admin-token`,
  `.local/runner-shared-secret`, and `.local/runner.yaml` are ignored private
  files. Never commit or paste their contents.

## Reproducible sequence

1. Use [step 21](21-local-smoke-preparation.md) to build images and binaries,
   install K3s, import **and tag** the agent image, apply manifests, and run
   `doctor --as-controller`. The doctor passed all checks on this host.
2. Start `.local/bin/spawnpod serve --as-controller --config .local/config.yaml`
   and wait until it logs `serving Forgejo plugin`. The Lease acquisition
   can take several seconds after restart.
3. Run `.local/bin/spawnpod smoke --config .local/config.yaml`. It passed
   Create, Start, CopyIn, Exec, CopyOut, and Remove with the real K3s API.
4. Start Forgejo using the Podman command in step 21. The helper
   [`bootstrap-forgejo.py`](../../../deploy/smoke/bootstrap-forgejo.py)
   installs the fresh instance and creates the local admin. Register the
   Runner using Forgejo's offline registration command with the private
   shared secret; [`configure-local-runner.py`](../../../deploy/smoke/configure-local-runner.py)
   writes the matching private Runner configuration. For a new local
   instance, the commands are:

   ```sh
   python3 deploy/smoke/bootstrap-forgejo.py \
     --password-file .local/forgejo-admin-password
   podman cp .local/runner-shared-secret \
     spawnpod-forgejo:/tmp/spawnpod-runner-secret
   podman exec --user root spawnpod-forgejo \
     chown git:git /tmp/spawnpod-runner-secret
   podman exec --user git spawnpod-forgejo forgejo forgejo-cli actions register \
     --secret-file /tmp/spawnpod-runner-secret --name spawnpod-local \
     --labels spawnpod
   ```

   Save the UUID printed by registration, without the shared secret, in
   `.local/runner-uuid` (mode `0600`), then run:

   ```sh
   python3 deploy/smoke/configure-local-runner.py \
     --template .local/runner.generated.yaml \
     --uuid-file .local/runner-uuid \
     --secret-file .local/runner-shared-secret \
     --output .local/runner.yaml
   ```
5. Run `.local/bin/forgejo-runner --config .local/runner.yaml daemon` and
   execute:

   ```sh
   python3 deploy/smoke/forgejo-workflow.py \
     --token-file .local/forgejo-admin-token \
     --workflow deploy/smoke/workflow.yml
   ```

   The helper creates or updates private repository
   `spawnpod-admin/spawnpod-smoke`, commits the workflow, dispatches it, and
   polls the run. Run 9 finished `success` using the local kubeconfig.
   Runs 11 and 12 also finished `success` with `serve --as-controller`: the
   first step wrote a file to `/workspace`, and the second read and checked
   that file in the same Pod.
6. `KUBECONFIG=.local/kubeconfig kubectl get pods -n ci` showed no remaining
   job Pod. The successful run environment records were `cleaned`;
   `environments cleanup --dry-run` had no
   candidates. `go test ./...` and `go vet ./...` passed.

## Findings and fixes from the run

- `ctr images import` must target the `k8s.io` namespace and the exact named
  digest reference must be tagged. A tag alone did not satisfy the Pod's
  digest image reference.
- Runner sends CopyIn destinations such as `/workspace/.act/`. Workspace
  validation now accepts a single trailing slash while maintaining the
  canonical-path and traversal checks.
- A terminal error from the agent can make its `pods/exec` process exit with
  status 1. The provider now preserves the terminal error, which revealed
  the actual command startup failure.
- Runner's default workdir was
  `/workspace/spawnpod-admin/spawnpod-smoke`. A workflow without checkout had
  not created that directory, so `sh` failed to start. The smoke workflow
  explicitly sets `defaults.run.working-directory: /workspace` and
  `defaults.run.shell: sh` for Alpine. Workflows using a repository workdir
  must create it, for example through an appropriate checkout step.
- The first run under impersonation (run 10) completed its first step but its
  second command failed when the Kubernetes client's rate limiter saw a
  cancelled context. Runs 11 and 12 passed unchanged. Later repeated runs
  exposed a reconciliation race; [step 23](23-live-policy-and-reconciliation.md)
  records its fix and subsequent validation.

## What this run does not establish

The smoke workflow has no service container, checkout, external action,
outbound network dependency, or concurrent job. Restart recovery, broader
NetworkPolicy enforcement, service startup and service networking still need
live scenarios before a production claim. Targeted admission, quota and
egress checks are recorded in [step 23](23-live-policy-and-reconciliation.md).
The local Runner source has protocol schema parity with SpawnPod, but its
exact tagged release provenance remains unverified.
