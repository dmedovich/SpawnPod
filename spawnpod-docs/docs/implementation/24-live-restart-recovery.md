# Implementation step 24 — controller restart recovery on K3s

**Date:** 2026-10-09. The controller used `--as-controller` against the local
K3s node. The [recovery RPC client](../../../deploy/smoke/recovery-client.go)
drives the same Unix plugin endpoint used by the Forgejo Runner.

## Ready environment across restart

The client created environment `3ff3ad5e347a78a968a3cb0068f417c2` and
waited for `StartComplete`. The controller was stopped cleanly. While it was
down, the ConfigMap record remained `ready` and the Pod remained `Running`.
After a new controller acquired the Lease, the same environment ID accepted
an `Exec` call and produced the expected output. `Remove` then marked the
record `cleaned` and deleted the Pod.

## Interrupted command after process crash

The client created environment `2ab4b75f47da88fe3370a082b1ce38a9` and
started a command that printed a start marker and then slept. The test killed
only the local controller process with SIGKILL. The client stream ended with
`Unavailable`; the durable record remained `executing` and the Pod was still
`Running`. A new controller acquired the Lease, performed its initial
reconciliation, marked the record `cleaned`, and deleted that Pod. A cleanup
preview then had no candidates.

The test client writes the current ID to ignored `.local/recovery-environment-id`
before Start. Its recovery modes are `create`, `exec`, `hold` and `remove`;
service modes are documented in [step 25](25-live-service-isolation.md). For a clean
restart test, use:

```sh
.local/bin/spawnpod-recovery-client -mode create -image APPROVED_DIGEST_IMAGE
# Stop and restart the controller; wait for its serving message.
.local/bin/spawnpod-recovery-client -mode exec
.local/bin/spawnpod-recovery-client -mode remove
```

For a crash test, use `-mode hold` after creation, wait for `command started`,
then terminate only the test controller process and restart it. Verify the
record is `executing` before restart and `cleaned` afterward. The local
test deliberately used SIGKILL; it is not a normal shutdown procedure.

These two cases establish the ready and interrupted-execution recovery paths.
They do not cover a crash during file transfer, partial service creation,
failed Kubernetes deletion, or Lease takeover by a second controller.
