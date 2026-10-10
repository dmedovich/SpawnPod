# Implementation step 32 — official Forgejo Runner v13.1.0 source

**Date:** 2026-10-10. This step checks a tagged Runner build directly.

## Source and build provenance

The official [Forgejo Runner v13.1.0 tag](https://code.forgejo.org/forgejo/runner/src/tag/v13.1.0)
resolves to commit `6095cb17bfdbded5aa4ea84c18a4b69fd9574cca`.
We cloned that tag into ignored `.local/runner-v13.1.0` and built
`.local/bin/forgejo-runner-v13.1.0` from it with Go 1.27.1 and the release
version linker setting. The binary reported `forgejo-runner version v13.1.0`;
its SHA-256 was
`7252ce76886ad6f7f7806aa9556e42d463307b046648e71d9be594f39cd32fb3`.
This is a local build of tagged source, not a checksum match against a
published release binary.

The old `/home/dmedovich/runner-main/runner` archive has no Git metadata and
differs from the tag in 45 paths. Its `act/plugin/adapter.go` adjusts the
Create timeout to the remaining context deadline, while the tagged file does
not. The `plugin.proto` files in the archive, official tag, and SpawnPod copy
are byte-identical (SHA-256
`961fc5fc541c5c79f5502632d9f6dd3daa110ecfd55b9fbf697778b873367f79`).
The archive's prior successful workflows therefore cannot be attributed to
the official v13.1.0 source; the runs below use the tagged build.

## Local integration matrix

Forgejo v15.0.9, local K3s, SpawnPod's `--as-controller` process and the
existing private Runner registration were used. The tagged Runner announced
itself to Forgejo as `v13.1.0`.

| Scenario | Workflow | Forgejo run | Result |
|---|---|---:|---|
| Create, Start, sequential Exec, shared workspace, Remove | [`workflow.yml`](../../../deploy/smoke/workflow.yml) | 25 | `success` |
| Approved service image, hostname, port and echo response | [`workflow-service.yml`](../../../deploy/smoke/workflow-service.yml) | 26 | `success` |
| Explicit approved job image, container env, `GITHUB_OUTPUT` and `GITHUB_ENV` across steps | [`workflow-runner-contract.yml`](../../../deploy/smoke/workflow-runner-contract.yml) | 27 | `success` |
| Nonzero command exit (`17`) | [`workflow-runner-failure.yml`](../../../deploy/smoke/workflow-runner-failure.yml) | 28 | `failure` as expected |
| Cancel a running command by starting a new run in the same [concurrency group](https://forgejo.org/docs/v15.0/user/actions/reference/#concurrency) | [`workflow-runner-cancel.yml`](../../../deploy/smoke/workflow-runner-cancel.yml) | 29, 30 | first `cancelled`, second `success` |

Run 29 was observed in `executing` before run 30 was dispatched. The tagged
Runner logged local task termination after Forgejo reported cancellation.
All six SpawnPod environment records were `cleaned`. The `ci` namespace
held zero Pods and Services and only its baseline default-deny NetworkPolicy;
the cleanup preview had no candidates.

```sh
git ls-remote --tags https://code.forgejo.org/forgejo/runner.git v13.1.0
git clone --depth 1 --branch v13.1.0 \
  https://code.forgejo.org/forgejo/runner.git .local/runner-v13.1.0
repo_root=$PWD
(
  cd .local/runner-v13.1.0
  go build -tags 'netgo osusergo' \
    -ldflags '-X code.forgejo.org/forgejo/runner/v13/internal/pkg/ver.version=v13.1.0' \
    -o "$repo_root/.local/bin/forgejo-runner-v13.1.0" .
)
.local/bin/forgejo-runner-v13.1.0 --version
# Keep this daemon running in another terminal:
.local/bin/forgejo-runner-v13.1.0 --config .local/runner.yaml daemon
python3 deploy/smoke/forgejo-workflow.py --token-file .local/forgejo-admin-token \
  --workflow deploy/smoke/workflow-runner-contract.yml
# For cancellation, dispatch the first run with --input delay=300. Once its
# environment is executing, dispatch the same workflow with --input delay=1.
```

The tagged Runner's CopyIn/CopyOut path was subsequently exercised by run 34;
run 35 additionally verified the default repository working directory with
the rebuilt agent; see [step 33](33-file-transfer-and-security.md). The
adapter's empty `ExecFailed` defense has a unit test. Alternate architectures
and broader path and image environment cases remain separate compatibility
checks.
