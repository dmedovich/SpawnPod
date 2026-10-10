# Implementation step 26 — service failure recovery on local K3s

**Date:** 2026-10-09. These tests used the same single-node K3s cluster,
`ci` namespace, approved Alpine image and Forgejo plugin RPC as [step 25](25-live-service-isolation.md).
The controller reconciled failed records every 30 seconds. Run these tests
only when no unrelated workload uses the `ci` namespace.

## Partial Kubernetes resource creation

The service provider creates egress and ingress NetworkPolicies before its
ClusterIP Service and Pod. Applying
[`service-quota-fail.yaml`](../../../deploy/smoke/service-quota-fail.yaml)
temporarily set `count/services: 0`, so Kubernetes rejected the Service after
both policies had been created. The RPC `Create` returned `Internal`. The
persisted record `cdcf6233610290fed1ac7520987fe1ba` contained UIDs for both
policies and no Service or Pod UID. Reconciliation moved it to `cleaned` and
removed both policies. The namespace then held zero Pods and Services and
only the baseline default-deny policy.

```sh
KUBECONFIG=.local/kubeconfig kubectl apply -f deploy/smoke/service-quota-fail.yaml
.local/bin/spawnpod-recovery-client -mode create-service \
  -id-file .local/recovery-partial-id -image "$APPROVED_ALPINE_DIGEST"
# The Create error is expected; find the newest spawnpod-recovery-smoke record
# with `spawnpod environments list --config .local/config.yaml`.
KUBECONFIG=.local/kubeconfig kubectl delete -f deploy/smoke/service-quota-fail.yaml
```

Remove the temporary quota even if the RPC test fails for another reason.
The failed RPC does not return an environment ID, so inspect the persisted
record by owner and creation time. `APPROVED_ALPINE_DIGEST` must be the digest
from the local approved profile.

## Service process fails to start

[`make-service-failure-config.py`](../../../deploy/smoke/make-service-failure-config.py)
copies the ignored local config to a private test config and replaces the
approved echo command with `/missing-service-binary`. This preserves the
service image, port and resource policy. `validate` passed. Restarting the
controller with this config and calling `create-service` produced another
`Internal` RPC error. Record `7476fdbffae984bce0d5b0532b95943e` was
observed in `failed` with `last_error: service startup failed`. Its handle
contained UIDs for the service Pod, ClusterIP Service and both policies.
All four resources were visible while the record was failed. The next
reconciliation set it to `cleaned` and removed them. Namespace quota returned
to zero Pods and Services and one baseline policy.

```sh
python3 deploy/smoke/make-service-failure-config.py
.local/bin/spawnpod validate --config .local/config.service-fail.yaml
# Stop the current controller, then start it with:
.local/bin/spawnpod serve --as-controller --config .local/config.service-fail.yaml
.local/bin/spawnpod-recovery-client -mode create-service \
  -id-file .local/recovery-start-fail-id -image "$APPROVED_ALPINE_DIGEST"
# The Create error is expected. Inspect the newest record and K3s resources.
# Stop this controller and restart serve with .local/config.yaml.
```

After restoring `.local/config.yaml`, `doctor --as-controller` passed. A
fresh approved echo service reached `ready`, returned the expected response
through the job Pod and was removed. The test quota was deleted. The final
namespace check showed zero Pods and Services, one baseline NetworkPolicy,
and the normal ResourceQuota. `environments cleanup --dry-run` returned no
candidates.

These tests demonstrate cleanup for two specific failure points. Service
process exit after a successful start is covered in [step 27](27-live-pod-and-service-loss.md).
A crash after all Kubernetes resources exist but before service readiness is
covered in [step 28](28-live-provision-crash.md). An interrupted individual
Kubernetes Create response and cleanup during an API outage remain untested.
