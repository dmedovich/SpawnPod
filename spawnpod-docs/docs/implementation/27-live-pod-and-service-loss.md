# Implementation step 27 — Pod and service loss on local K3s

**Date:** 2026-10-09. The local controller used the `ci` namespace and the
approved Alpine profile from [step 25](25-live-service-isolation.md).

## Job Pod disappears during Exec

The [recovery RPC client](../../../deploy/smoke/recovery-client.go) created
environment `329763a643a32e07fb6ee4870dc976e2` and started its `hold`
command. After the client printed `command started`, the test deleted that
environment's job Pod with `kubectl delete pod --wait=false`. The Exec stream
ended with `Exec failed: execution failed`. The record became `cleaned` and
the Pod was absent; it was not recreated or replayed.

```sh
.local/bin/spawnpod-recovery-client -mode create \
  -id-file .local/recovery-pod-loss-id -image "$APPROVED_ALPINE_DIGEST"
.local/bin/spawnpod-recovery-client -mode hold \
  -id-file .local/recovery-pod-loss-id
# After "command started", delete only the Pod named sp-<record ID> in ci.
```

## Service process exits after readiness

A service environment `4b470b3e76b3c7aa96e20bd0aa9cd25e` reached
`ready`. The test used `kubectl exec` in its service Pod to stop the BusyBox
`nc` process. The service Pod became `Failed` and a service echo command
failed, but the environment record returned to `ready`: reconciliation had
only checked the deadline and state. We removed this test environment.

The engine now checks the recorded service Pod and Service for idle ready
environments during each reconciliation. A missing Pod or Service, or a Pod
in terminal `Failed`/`Succeeded` phase, triggers UID-checked cleanup. An API
error leaves the record intact for a later retry; an ownership or definition
mismatch remains visible as an error rather than authorizing deletion of an
unrelated resource. The check does not treat a temporary readiness failure
as terminal loss.

After rebuilding and restarting the controller, environment
`169dc55b1b8e1802129ec494bda6888a` reached `ready`. We stopped its
`nc` process in the same way. On the next reconciliation, the record became
`cleaned`; its job Pod, service Pod, ClusterIP Service and two NetworkPolicies
were gone. The namespace held zero Pods and Services and only the baseline
default-deny policy. Engine and provider tests cover healthy, terminal and
API-error results. `go test ./...` and `go vet ./...` passed.

This test covers terminal service Pod loss. A separate probe would be needed
to detect a process that remains running but no longer serves traffic.
