# Implementation step 30 — deletion delayed by a Pod finalizer

**Date:** 2026-10-09. This local K3s test verified that an incomplete Pod
deletion stays visible and can finish after the blocking condition is removed.
It used only a test environment in the `ci` namespace.

[`make-delete-delay-config.py`](../../../deploy/smoke/make-delete-delay-config.py)
copies the ignored local config to a private file and changes the controller
cleanup timeout from two minutes to eight seconds. `validate` passed. We
started the controller with this config and created environment
`d16b44a7a8769b1bf5c3f59cc9687a99` through the Forgejo plugin RPC.
An admin `kubectl patch` added the temporary finalizer
`spawnpod.dev/smoke-hold` only to its job Pod.

`Remove` requested Pod deletion but returned `DeadlineExceeded` because the
finalizer kept the Pod in `Terminating`. The persistent environment record
remained `terminating`; the controller logged a reconciliation timeout on a
later attempt. We removed that finalizer. Kubernetes deleted the Pod and the
controller's next retry moved the record to `cleaned`. The final namespace
contained zero Pods and Services and only the baseline default-deny policy.

```sh
python3 deploy/smoke/make-delete-delay-config.py
.local/bin/spawnpod validate --config .local/config.delete-fail.yaml
# Stop the ordinary controller and start this temporary one:
.local/bin/spawnpod serve --as-controller --config .local/config.delete-fail.yaml
.local/bin/spawnpod-recovery-client -mode create \
  -id-file .local/recovery-delete-fail-id -image "$APPROVED_ALPINE_DIGEST"
environment_id=$(cat .local/recovery-delete-fail-id)
KUBECONFIG=.local/kubeconfig kubectl patch pod "sp-$environment_id" -n ci --type=merge \
  -p '{"metadata":{"finalizers":["spawnpod.dev/smoke-hold"]}}'
.local/bin/spawnpod-recovery-client -mode remove \
  -id-file .local/recovery-delete-fail-id
# The timeout is expected. Remove the test finalizer even if Remove fails.
KUBECONFIG=.local/kubeconfig kubectl patch pod "sp-$environment_id" -n ci --type=merge \
  -p '{"metadata":{"finalizers":[]}}'
# Verify the record becomes cleaned, then restore .local/config.yaml.
```

The finalizer simulates a deletion that was accepted by Kubernetes but could
not complete. It does not test an API rejection of the Delete request or
permanent node unavailability. `APPROVED_ALPINE_DIGEST` must be the digest
from the local approved profile.
