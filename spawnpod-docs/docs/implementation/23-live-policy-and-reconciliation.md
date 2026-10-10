# Implementation step 23 — live policy checks and reconciliation race

**Date:** 2026-10-09. **Environment:** local Fedora 43/K3s v1.34.12+k3s1,
Forgejo v15.0.9, local Runner source reporting `version dev`.

## Admission and quota

[`check-cluster-policy.py`](../../../deploy/smoke/check-cluster-policy.py)
uses Kubernetes server-side dry runs for a valid job Pod, a job Pod with an
unapproved image, and a privileged job Pod. It then checks ConfigMap quota
enforcement and removes every resource created for the test. Run it with:

```sh
python3 deploy/smoke/check-cluster-policy.py \
  --kubeconfig .local/kubeconfig --config .local/config.yaml
```

Observed results:

- The approved Pod shape was admitted in server-side dry run.
- ValidatingAdmissionPolicy `spawnpod-workload` denied the unapproved image.
- Restricted Pod Security v1.34 denied the privileged container.
- ResourceQuota denied an extra ConfigMap. With the former 16-slot local
  budget, the check filled two remaining slots with temporary ConfigMaps and
  removed them. With the new 64-slot budget, it used a temporary zero-slot
  ResourceQuota, confirmed denial, and removed that quota.

The local 16-ConfigMap quota filled after repeated workflow runs because
cleaned environment records remain as tombstones for up to 24 hours. The
smoke generator and active local config now use 64 slots. The generated
ResourceQuota was reapplied; `doctor --as-controller` passed after the change.
Production quota sizing still needs a job-rate and retention calculation.

## Network isolation

The `kubernetes` Service in this cluster has ClusterIP `10.43.0.1`. A
temporary Alpine control Pod in the unrestricted `default` namespace opened
TCP port 443 on that Service using `/bin/busybox nc -z -w 3`. The
[`workflow-network.yml`](../../../deploy/smoke/workflow-network.yml)
ran inside a SpawnPod job Pod in `ci` and required that same connection to
fail. Forgejo runs 13 and 14 finished `success`; the control Pod was deleted
afterward. This establishes egress denial to the API Service for the tested
job Pod. It does not establish every ingress or egress case, service-network
permissions, DNS behavior, or behavior under a different CNI.

The workflow helper now derives the repository workflow path from the input
filename. This allows the ordinary and network workflows to be dispatched
independently:

```sh
python3 deploy/smoke/forgejo-workflow.py \
  --token-file .local/forgejo-admin-token \
  --workflow deploy/smoke/workflow-network.yml
```

## Reconciliation race and fix

Intermittent Forgejo runs 10, 15 and 19 failed between steps with
`environment is not in the required state`. The failures clustered around
the 30-second reconciliation sweep. Inspection found two related windows:

1. `ReconcileOnce` listed a transition-state record and could act on that
   stale snapshot after the record had become `ready`. It now re-reads the
   record and claims cleanup through a revision-checked transition.
2. Successful `Exec` and `CopyIn` released their active-operation slot before
   persisting `ready`. A sweep in that window interpreted the record as an
   interrupted operation. Both methods now hold the slot until the `ready`
   update completes. Error paths record `indeterminate` before releasing it.

The engine tests reproduce a stale list, a concurrent revision change, and a
CopyIn blocked immediately before recording `ready`. The full `go test ./...`
and `go vet ./...` passed. After rebuilding and restarting the controller
with ServiceAccount impersonation, four consecutive two-step Forgejo runs
(20–23) passed across multiple sweeps. The remaining job Pod count was zero.
Four runs reduce the observed regression risk; longer soak and restart tests
are still required for a reliability claim.
