# Implementation step 33 — tagged Runner file transfer and live security checks

**Date:** 2026-10-10. **Environment:** Forgejo v15.0.9, official Runner
v13.1.0 at commit `6095cb17bfdbded5aa4ea84c18a4b69fd9574cca`, and
single-node K3s v1.34.12+k3s1 on `amd64`.

## Runner file transfer

[`workflow-runner-files.yml`](../../../deploy/smoke/workflow-runner-files.yml)
loads a small composite action from the local Forgejo repository. The tagged
Runner's `maybeCopyToActionDir` calls `CopyDir`, whose plugin implementation
streams `CopyIn`; the action reads its copied `marker.txt` in the Pod. The
workflow then writes `GITHUB_OUTPUT`, which the tagged Runner reads through
`GetContainerArchive`/`CopyOut`, and checks the resulting step output. Forgejo
run 34 finished `success`. This verifies both file directions in one real
Runner job with a nonempty copied action directory. Previous run 27 also
exercised output and environment files across steps.

The action files are uploaded before dispatch by the `--asset` option of
[`forgejo-workflow.py`](../../../deploy/smoke/forgejo-workflow.py):

```sh
python3 deploy/smoke/forgejo-workflow.py \
  --token-file .local/forgejo-admin-token \
  --asset deploy/smoke/file-transfer-action.yml=.forgejo/actions/file-transfer/action.yml \
  --asset deploy/smoke/file-transfer-marker.txt=.forgejo/actions/file-transfer/marker.txt \
  --workflow deploy/smoke/workflow-runner-files.yml
```

Runs 31–33 exposed two test setup constraints. The Runner daemon executes
`actions/checkout@v4` as a Node action; the approved Alpine smoke image has no
Node. The remote composite action initially failed because its default
`/workspace/spawnpod-admin/spawnpod-smoke` working directory did not exist.
Run 34 explicitly created it before the action. The agent now has
`workspace.EnsureDirectory`, using `os.Root` and canonical path checks, to
create that directory before invoking a command. Its unit test verifies
creation and refusal of traversal and symlink paths. A rebuilt local image
`localhost/spawnpod-agent:smoke-workdir` has manifest digest
`sha256:52c5b60152e83fb84f4ff60dbcba2b0d5b2083e0e111d8a7aaf635223bd1ee7a`.
The image was imported into K3s as
`localhost/spawnpod-agent@sha256:52c5b60152e83fb84f4ff60dbcba2b0d5b2083e0e111d8a7aaf635223bd1ee7a`.
The local config and generated admission allowlist were updated to that digest;
`doctor --as-controller` passed. After restarting the controller,
[`workflow-runner-default-workdir.yml`](../../../deploy/smoke/workflow-runner-default-workdir.yml)
ran without an explicit `mkdir` step. Tagged Runner run 35 finished `success`.
Its environment record reached `cleaned` shortly after Forgejo reported
success, and no Pod or Service remained in `ci`. This verifies the new
workdir behavior and the copied action path on the rebuilt image.

## Live security results

[`check-cluster-policy.py`](../../../deploy/smoke/check-cluster-policy.py)
passed server-side dry runs for an approved Pod and denials for unapproved
image, privileged container, host network, hostPath, Secret volume, Secret
environment variable and host port. `SelfSubjectAccessReview` denied the
controller access to Secrets and Pod creation outside `ci`, and denied the
job ServiceAccount access to Secrets and Pod listing. The controller retained
Pod creation permission in `ci`. A temporary zero-slot ResourceQuota denied
an extra ConfigMap and was removed.

The [recovery client](../../../deploy/smoke/recovery-client.go) sent a tar
member named `../escape` through the real `CopyIn` RPC. The RPC returned
`Internal` with a generic client message, while the controller log recorded
`invalid archive entry path` from the agent. The environment was cleaned;
repeat `Remove` succeeded. The `ci` namespace ended with no Pods or Services
and only the baseline default-deny NetworkPolicy.

[`check-live-pod.py`](../../../deploy/smoke/check-live-pod.py) inspected a
running job Pod and confirmed the 2 GiB workspace `emptyDir` size limit,
numeric UID 10001, `runAsNonRoot`, read-only root filesystems, dropped
capabilities, RuntimeDefault seccomp, disabled privilege escalation, no
ServiceAccount token mount, no Secret mounts and no PodSpec environment.
The Pod was removed after inspection.

`go test ./...` and `go vet ./...` passed outside the restricted command
sandbox. The sandbox's Go cache and Unix socket restrictions prevented the
first full test attempt; that attempt is not counted as a code failure.
The Forgejo adapter test also verifies that a backend ending without a
terminal event yields a nonempty `ExecFailed` message.
The expanded admission, RBAC and quota checks passed again after the image
digest and policy change.

## Remaining evidence for the first release gate

- Source proxy and its K3s egress policy were added in
  [step 34](34-source-proxy.md). A complete HTTPS checkout with the official
  Runner is still unverified.
- Only an `amd64` node is available. An `arm64` cluster or node is required
  for a real second-architecture run. A static Linux ARM64 agent binary was
  cross-built, which does not verify its behavior in a Pod.
- Test more path and image environment variants if the supported contract is
  expanded beyond the cases above. The empty `ExecFailed` defense is covered
  by an adapter test; a real Runner error-path workflow is still useful.
